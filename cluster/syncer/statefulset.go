/*
Copyright 2021 RadonDB.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package syncer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/iancoleman/strcase"
	"github.com/imdario/mergo"
	"github.com/presslabs/controller-util/mergo/transformers"
	"github.com/presslabs/controller-util/syncer"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/radondb/radondb-mysql-kubernetes/cluster"
	"github.com/radondb/radondb-mysql-kubernetes/cluster/container"
	"github.com/radondb/radondb-mysql-kubernetes/internal"
	"github.com/radondb/radondb-mysql-kubernetes/utils"
)

// The wait time limit for pod upgrade.
const waitLimit = 2 * 60 * 60

// StatefulSetSyncer used to operate statefulset.
type StatefulSetSyncer struct {
	*cluster.Cluster

	cli client.Client

	sfs *appsv1.StatefulSet

	// Configmap resourceVersion.
	cmRev string

	// Secret resourceVersion.
	sctRev string
}

// NewStatefulSetSyncer returns a pointer to StatefulSetSyncer.
func NewStatefulSetSyncer(cli client.Client, c *cluster.Cluster, cmRev, sctRev string) *StatefulSetSyncer {
	return &StatefulSetSyncer{
		Cluster: c,
		cli:     cli,
		sfs: &appsv1.StatefulSet{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "StatefulSet",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      c.GetNameForResource(utils.StatefulSet),
				Namespace: c.Namespace,
			},
		},
		cmRev:  cmRev,
		sctRev: sctRev,
	}
}

// Object returns the object for which sync applies.
func (s *StatefulSetSyncer) Object() interface{} { return s.sfs }

// GetObject returns the object for which sync applies
func (s *StatefulSetSyncer) GetObject() interface{} { return s.sfs }

// Owner returns the object owner or nil if object does not have one.
func (s *StatefulSetSyncer) ObjectOwner() runtime.Object { return s.Unwrap() }

// GetOwner returns the object owner or nil if object does not have one.
func (s *StatefulSetSyncer) GetOwner() runtime.Object { return s.Unwrap() }

// Sync persists data into the external store.
// It's called by cluster controller, when return error, retry Reconcile(),when return nil, exit this cycle.
// See https://github.com/presslabs/controller-util/blob/master/syncer/object.go#L68
func (s *StatefulSetSyncer) Sync(ctx context.Context) (syncer.SyncResult, error) {
	var err error
	var kind string
	result := syncer.SyncResult{}

	result.Operation, err = s.createOrUpdate(ctx)

	// Get namespace and name.
	key := client.ObjectKeyFromObject(s.sfs)
	// Get groupVersionKind.
	gvk, gvkErr := apiutil.GVKForObject(s.sfs, s.cli.Scheme())
	if gvkErr != nil {
		kind = fmt.Sprintf("%T", s.sfs)
	} else {
		kind = gvk.String()
	}
	// Print log.
	// Info: owner is deleted or ignored error.
	// Warning: other errors.
	// Normal: no error.
	switch {
	case errors.Is(err, syncer.ErrOwnerDeleted):
		log.Info(string(result.Operation), "key", key, "kind", kind, "error", err)
		err = nil
	case errors.Is(err, syncer.ErrIgnore):
		log.Info("syncer skipped", "key", key, "kind", kind, "error", err)
		err = nil
	case err != nil:
		result.SetEventData("Warning", basicEventReason(s.Name, err),
			fmt.Sprintf("%s %s failed syncing: %s", kind, key, err))
		log.Error(err, string(result.Operation), "key", key, "kind", kind)
	default:
		result.SetEventData("Normal", basicEventReason(s.Name, err),
			fmt.Sprintf("%s %s %s successfully", kind, key, result.Operation))
		log.Info(string(result.Operation), "key", key, "kind", kind)
	}
	return result, err
}

// createOrUpdate creates or updates the statefulset in the Kubernetes cluster.
// See https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.9.2/pkg/controller/controllerutil?utm_source=gopls#CreateOrUpdate
func (s *StatefulSetSyncer) createOrUpdate(ctx context.Context) (controllerutil.OperationResult, error) {
	var err error
	// Check if statefulset exists
	if err = s.cli.Get(ctx, client.ObjectKeyFromObject(s.sfs), s.sfs); err != nil {
		if !k8serrors.IsNotFound(err) {
			return controllerutil.OperationResultNone, err
		}

		if err = s.mutate(); err != nil {
			return controllerutil.OperationResultNone, err
		}

		if err = s.cli.Create(ctx, s.sfs); err != nil {
			return controllerutil.OperationResultNone, err
		} else {
			return controllerutil.OperationResultCreated, nil
		}
	}
	// Deep copy the old statefulset from StatefulSetSyncer.
	existing := s.sfs.DeepCopyObject()
	// Sync data from cluster.spec to statefulset.
	if err = s.mutate(); err != nil {
		return controllerutil.OperationResultNone, err
	}
	// Check if statefulset changed.
	if equality.Semantic.DeepEqual(existing, s.sfs) {
		return controllerutil.OperationResultNone, nil
	}
	// If changed, update statefulset.
	if err := s.cli.Update(ctx, s.sfs); err != nil {
		return controllerutil.OperationResultNone, err
	}
	// Update every pods of statefulset.
	if err := s.updatePod(ctx); err != nil {
		return controllerutil.OperationResultNone, err
	}
	// Update pvc.
	if err := s.updatePVC(ctx); err != nil {
		return controllerutil.OperationResultNone, err
	}
	return controllerutil.OperationResultUpdated, nil
}

// updatePod update the pods, update follower nodes first.
// This can reduce the number of master-slave switching during the update process.
func (s *StatefulSetSyncer) updatePod(ctx context.Context) error {
	if s.sfs.Status.UpdatedReplicas >= s.sfs.Status.Replicas {
		return nil
	}

	log.Info("statefulSet was changed, run update")

	if s.sfs.Status.ReadyReplicas < s.sfs.Status.Replicas {
		log.Info("can't start/continue 'update': waiting for all replicas are ready")
		return nil
	}
	// Get all pods.
	pods := corev1.PodList{}
	if err := s.cli.List(ctx,
		&pods,
		&client.ListOptions{
			Namespace:     s.sfs.Namespace,
			LabelSelector: s.GetLabels().AsSelector(),
		},
	); err != nil {
		return err
	}
	var leaderPod corev1.Pod
	var followerPods []corev1.Pod
	for _, pod := range pods.Items {
		// Check if the pod is healthy.
		if pod.ObjectMeta.Labels["healthy"] != "yes" {
			return fmt.Errorf("can't start/continue 'update': pod[%s] is unhealthy", pod.Name)
		}
		// Skip if pod is leader.
		if pod.ObjectMeta.Labels["role"] == "leader" && leaderPod.Name == "" {
			leaderPod = pod
			continue
		}
		followerPods = append(followerPods, pod)
		// If pod is not leader, direct update.
		if err := s.applyNWait(ctx, &pod); err != nil {
			return err
		}
	}
	// All followers have been updated now, then update leader.
	if leaderPod.Name != "" {
		// When replicas is two (one leader and one follower).
		if len(followerPods) == 1 {
			if err := s.preUpdate(ctx, leaderPod.Name, followerPods[0].Name); err != nil {
				return err
			}
		}
		// Update the leader.
		if err := s.applyNWait(ctx, &leaderPod); err != nil {
			return err
		}
	}

	return nil
}

// preUpdate run before update the leader pod when replicas is 2.
// Its main function is manually switch the leader node.
// 1. Get secrets (operator-user, operator-password, root-password).
// 2. Connect leader mysql.
// 3. Set leader read only.
// 4. Make sure the leader has sent all binlog to follower.
// 5. Check followerHost current role.
// 6. If followerHost is not leader, switch it to leader through xenon.
func (s *StatefulSetSyncer) preUpdate(ctx context.Context, leader, follower string) error {
	if s.sfs.Status.Replicas != 2 {
		return nil
	}

	sctName := s.GetNameForResource(utils.Secret)
	svcName := s.GetNameForResource(utils.HeadlessSVC)
	port := utils.MysqlPort
	nameSpace := s.Namespace

	// Get secrets.
	secret := &corev1.Secret{}
	if err := s.cli.Get(context.TODO(),
		types.NamespacedName{
			Namespace: nameSpace,
			Name:      sctName,
		},
		secret,
	); err != nil {
		return fmt.Errorf("failed to get the secret: %s", sctName)
	}
	user, ok := secret.Data["operator-user"]
	if !ok {
		return fmt.Errorf("failed to get the user: %s", user)
	}
	password, ok := secret.Data["operator-password"]
	if !ok {
		return fmt.Errorf("failed to get the password: %s", password)
	}
	rootPasswd, ok := secret.Data["root-password"]
	if !ok {
		return fmt.Errorf("failed to get the root password: %s", rootPasswd)
	}

	leaderHost := fmt.Sprintf("%s.%s.%s", leader, svcName, nameSpace)
	leaderRunner, err := internal.NewSQLRunner(utils.BytesToString(user), utils.BytesToString(password), leaderHost, port)
	if err != nil {
		log.Error(err, "failed to connect the mysql", "node", leader)
		return err
	}
	defer leaderRunner.Close()

	if err = retry(time.Second*2, time.Duration(waitLimit)*time.Second, func() (bool, error) {
		// Set leader read only.
		if err = leaderRunner.RunQuery("SET GLOBAL super_read_only=on;"); err != nil {
			log.Error(err, "failed to set leader read only", "node", leader)
			return false, err
		}

		// Make sure the master has sent all binlog to slave.
		success, err := leaderRunner.CheckProcesslist()
		if err != nil {
			return false, err
		}
		if success {
			return true, nil
		}
		return false, nil
	}); err != nil {
		return err
	}

	followerHost := fmt.Sprintf("%s.%s.%s", follower, svcName, nameSpace)
	if err = retry(time.Second*5, time.Second*60, func() (bool, error) {
		// Check whether is leader.
		status, err := checkRole(followerHost, rootPasswd)
		if err != nil {
			log.Error(err, "failed to check role", "pod", follower)
			return false, nil
		}
		if status == corev1.ConditionTrue {
			return true, nil
		}

		// If not leader, try to leader.
		xenonHttpRequest(followerHost, "POST", "/v1/raft/trytoleader", rootPasswd, nil)
		return false, nil
	}); err != nil {
		return err
	}

	return nil
}

// mutate set the statefulset.
func (s *StatefulSetSyncer) mutate() error {
	s.sfs.Spec.ServiceName = s.GetNameForResource(utils.StatefulSet)
	s.sfs.Spec.Replicas = s.Spec.Replicas
	s.sfs.Spec.Selector = metav1.SetAsLabelSelector(s.GetSelectorLabels())
	s.sfs.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type: appsv1.OnDeleteStatefulSetStrategyType,
	}

	s.sfs.Spec.Template.ObjectMeta.Labels = s.GetLabels()
	for k, v := range s.Spec.PodSpec.Labels {
		s.sfs.Spec.Template.ObjectMeta.Labels[k] = v
	}
	s.sfs.Spec.Template.ObjectMeta.Labels["role"] = "candidate"
	s.sfs.Spec.Template.ObjectMeta.Labels["healthy"] = "no"

	s.sfs.Spec.Template.Annotations = s.Spec.PodSpec.Annotations
	if len(s.sfs.Spec.Template.ObjectMeta.Annotations) == 0 {
		s.sfs.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
	}
	if s.Spec.MetricsOpts.Enabled {
		s.sfs.Spec.Template.ObjectMeta.Annotations["prometheus.io/scrape"] = "true"
		s.sfs.Spec.Template.ObjectMeta.Annotations["prometheus.io/port"] = fmt.Sprintf("%d", utils.MetricsPort)
	}
	s.sfs.Spec.Template.ObjectMeta.Annotations["config_rev"] = s.cmRev
	s.sfs.Spec.Template.ObjectMeta.Annotations["secret_rev"] = s.sctRev

	err := mergo.Merge(&s.sfs.Spec.Template.Spec, s.ensurePodSpec(), mergo.WithTransformers(transformers.PodSpec))
	if err != nil {
		return err
	}
	s.sfs.Spec.Template.Spec.Tolerations = s.Spec.PodSpec.Tolerations

	if s.Spec.Persistence.Enabled {
		if s.sfs.Spec.VolumeClaimTemplates, err = s.EnsureVolumeClaimTemplates(s.cli.Scheme()); err != nil {
			return err
		}
	}

	// Set owner reference only if owner resource is not being deleted, otherwise the owner
	// reference will be reset in case of deleting with cascade=false.
	if s.Unwrap().GetDeletionTimestamp().IsZero() {
		if err := controllerutil.SetControllerReference(s.Unwrap(), s.sfs, s.cli.Scheme()); err != nil {
			return err
		}
	} else if ctime := s.Unwrap().GetCreationTimestamp(); ctime.IsZero() {
		// The owner is deleted, don't recreate the resource if does not exist, because gc
		// will not delete it again because has no owner reference set.
		return fmt.Errorf("owner is deleted")
	}
	return nil
}

// ensurePodSpec used to ensure the podspec.
func (s *StatefulSetSyncer) ensurePodSpec() corev1.PodSpec {
	initSidecar := container.EnsureContainer(utils.ContainerInitSidecarName, s.Cluster)
	initMysql := container.EnsureContainer(utils.ContainerInitMysqlName, s.Cluster)
	initContainers := []corev1.Container{initSidecar, initMysql}

	mysql := container.EnsureContainer(utils.ContainerMysqlName, s.Cluster)
	xenon := container.EnsureContainer(utils.ContainerXenonName, s.Cluster)
	containers := []corev1.Container{mysql, xenon}
	if s.Spec.MetricsOpts.Enabled {
		containers = append(containers, container.EnsureContainer(utils.ContainerMetricsName, s.Cluster))
	}
	if s.Spec.PodSpec.SlowLogTail {
		containers = append(containers, container.EnsureContainer(utils.ContainerSlowLogName, s.Cluster))
	}
	if s.Spec.PodSpec.AuditLogTail {
		containers = append(containers, container.EnsureContainer(utils.ContainerAuditLogName, s.Cluster))
	}

	return corev1.PodSpec{
		InitContainers:     initContainers,
		Containers:         containers,
		Volumes:            s.EnsureVolumes(),
		SchedulerName:      s.Spec.PodSpec.SchedulerName,
		ServiceAccountName: s.GetNameForResource(utils.ServiceAccount),
		Affinity:           s.Spec.PodSpec.Affinity,
		PriorityClassName:  s.Spec.PodSpec.PriorityClassName,
		Tolerations:        s.Spec.PodSpec.Tolerations,
	}
}

// updatePVC used to update the pvc, check and remove the extra pvc.
func (s *StatefulSetSyncer) updatePVC(ctx context.Context) error {
	pvcs := corev1.PersistentVolumeClaimList{}
	if err := s.cli.List(ctx,
		&pvcs,
		&client.ListOptions{
			Namespace:     s.sfs.Namespace,
			LabelSelector: s.GetLabels().AsSelector(),
		},
	); err != nil {
		return err
	}

	for _, item := range pvcs.Items {
		if item.DeletionTimestamp != nil {
			log.Info("pvc is being deleted", "pvc", item.Name, "key", s.Unwrap())
			continue
		}

		ordinal, err := utils.GetOrdinal(item.Name)
		if err != nil {
			log.Error(err, "pvc deletion error", "key", s.Unwrap())
			continue
		}

		if ordinal >= int(*s.Spec.Replicas) {
			log.Info("cleaning up pvc", "pvc", item.Name, "key", s.Unwrap())
			if err := s.cli.Delete(ctx, &item); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *StatefulSetSyncer) applyNWait(ctx context.Context, pod *corev1.Pod) error {
	// Check version, if not latest, delete node.
	if pod.ObjectMeta.Labels["controller-revision-hash"] == s.sfs.Status.UpdateRevision {
		log.Info("pod is already updated", "pod name", pod.Name)
	} else {
		log.Info("updating pod", "pod", pod.Name, "key", s.Unwrap())
		if pod.DeletionTimestamp != nil {
			log.Info("pod is being deleted", "pod", pod.Name, "key", s.Unwrap())
		} else {
			if err := s.cli.Delete(ctx, pod); err != nil {
				return err
			}
		}
	}

	// Wait the pod restart and healthy.
	return retry(time.Second*10, time.Duration(waitLimit)*time.Second, func() (bool, error) {
		err := s.cli.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, pod)
		if err != nil && !k8serrors.IsNotFound(err) {
			return false, err
		}

		ordinal, err := utils.GetOrdinal(pod.Name)
		if err != nil {
			return false, err
		}
		if ordinal >= int(*s.Spec.Replicas) {
			log.Info("replicas were changed,  should skip", "pod", pod.Name)
			return true, nil
		}

		if pod.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("pod %s is in failed phase", pod.Name)
		}

		if pod.ObjectMeta.Labels["healthy"] == "yes" &&
			pod.ObjectMeta.Labels["controller-revision-hash"] == s.sfs.Status.UpdateRevision {
			return true, nil
		}

		return false, nil
	})
}

// retry runs func "f" every "in" time until "limit" is reached.
// it also doesn't have an extra tail wait after the limit is reached
// and f func runs first time instantly
func retry(in, limit time.Duration, f func() (bool, error)) error {
	fdone, err := f()
	if err != nil {
		return err
	}
	if fdone {
		return nil
	}

	done := time.NewTimer(limit)
	defer done.Stop()
	tk := time.NewTicker(in)
	defer tk.Stop()

	for {
		select {
		case <-done.C:
			return fmt.Errorf("reach pod wait limit")
		case <-tk.C:
			fdone, err := f()
			if err != nil {
				return err
			}
			if fdone {
				return nil
			}
		}
	}
}

func basicEventReason(objKindName string, err error) string {
	if err != nil {
		return fmt.Sprintf("%sSyncFailed", strcase.ToCamel(objKindName))
	}

	return fmt.Sprintf("%sSyncSuccessfull", strcase.ToCamel(objKindName))
}

func xenonHttpRequest(host, method, url string, rootPasswd []byte, body io.Reader) (io.ReadCloser, error) {
	req, err := http.NewRequest(method, fmt.Sprintf("http://%s:%d%s", host, utils.XenonPeerPort, url), body)
	if err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(append([]byte("root:"), rootPasswd...))
	req.Header.Set("Authorization", "Basic "+encoded)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("get raft status failed, status code is %d", resp.StatusCode)
	}

	return resp.Body, nil
}
