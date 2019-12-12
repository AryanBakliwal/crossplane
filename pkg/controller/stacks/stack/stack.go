/*
Copyright 2019 The Crossplane Authors.

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

package stack

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
	apps "k8s.io/api/apps/v1"
	batch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	runtimev1alpha1 "github.com/crossplaneio/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplaneio/crossplane-runtime/pkg/logging"
	"github.com/crossplaneio/crossplane-runtime/pkg/meta"
	runtimeresource "github.com/crossplaneio/crossplane-runtime/pkg/resource"
	"github.com/crossplaneio/crossplane/apis/stacks/v1alpha1"
	"github.com/crossplaneio/crossplane/pkg/stacks"
)

const (
	controllerName  = "stack.stacks.crossplane.io"
	stacksFinalizer = "finalizer.stacks.crossplane.io"

	reconcileTimeout      = 1 * time.Minute
	requeueAfterOnSuccess = 10 * time.Second
)

var (
	log              = logging.Logger.WithName(controllerName)
	resultRequeue    = reconcile.Result{Requeue: true}
	requeueOnSuccess = reconcile.Result{RequeueAfter: requeueAfterOnSuccess}

	roleVerbs = map[string][]string{
		"admin": {"get", "list", "watch", "create", "delete", "deletecollection", "patch", "update"},
		"edit":  {"get", "list", "watch", "create", "delete", "deletecollection", "patch", "update"},
		"view":  {"get", "list", "watch"},
	}
)

// Reconciler reconciles a Instance object
type Reconciler struct {
	kube client.Client
	factory
}

// Controller is responsible for adding the Stack
// controller and its corresponding reconciler to the manager with any runtime configuration.
type Controller struct{}

// SetupWithManager creates a new Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	r := &Reconciler{
		kube:    mgr.GetClient(),
		factory: &stackHandlerFactory{},
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(&v1alpha1.Stack{}).
		Complete(r)
}

// Reconcile reads that state of the Stack for a Instance object and makes changes based on the state read
// and what is in the Instance.Spec
func (r *Reconciler) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	log.V(logging.Debug).Info("reconciling", "kind", v1alpha1.StackKindAPIVersion, "request", req)

	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()

	// fetch the CRD instance
	i := &v1alpha1.Stack{}
	if err := r.kube.Get(ctx, req.NamespacedName, i); err != nil {
		if kerrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	handler := r.factory.newHandler(ctx, i, r.kube)

	return handler.sync(ctx)
}

type handler interface {
	sync(context.Context) (reconcile.Result, error)
	create(context.Context) (reconcile.Result, error)
	update(context.Context) (reconcile.Result, error)
	delete(context.Context) (reconcile.Result, error)
}

type stackHandler struct {
	kube client.Client
	ext  *v1alpha1.Stack
}

type factory interface {
	newHandler(context.Context, *v1alpha1.Stack, client.Client) handler
}

type stackHandlerFactory struct{}

func (f *stackHandlerFactory) newHandler(ctx context.Context, ext *v1alpha1.Stack, kube client.Client) handler {
	return &stackHandler{
		kube: kube,
		ext:  ext,
	}
}

// ************************************************************************************************
// Syncing/Creating functions
// ************************************************************************************************
func (h *stackHandler) sync(ctx context.Context) (reconcile.Result, error) {
	if h.ext.Status.ControllerRef == nil {
		return h.create(ctx)
	}

	return h.update(ctx)
}

func (h *stackHandler) create(ctx context.Context) (reconcile.Result, error) {
	h.ext.Status.SetConditions(runtimev1alpha1.Creating())

	meta.AddFinalizer(h.ext, stacksFinalizer)
	if err := h.kube.Update(ctx, h.ext); err != nil {
		return fail(ctx, h.kube, h.ext, err)
	}

	// create RBAC permissions
	if err := h.processRBAC(ctx); err != nil {
		return fail(ctx, h.kube, h.ext, err)
	}

	// create controller deployment or job
	if err := h.processDeployment(ctx); err != nil {
		return fail(ctx, h.kube, h.ext, err)
	}

	if err := h.processJob(ctx); err != nil {
		return fail(ctx, h.kube, h.ext, err)
	}

	// the stack has successfully been created, the stack is ready
	h.ext.Status.SetConditions(runtimev1alpha1.Available(), runtimev1alpha1.ReconcileSuccess())
	return requeueOnSuccess, h.kube.Status().Update(ctx, h.ext)
}

func (h *stackHandler) update(ctx context.Context) (reconcile.Result, error) {
	log.V(logging.Debug).Info("updating not supported yet", "stack", h.ext.Name)
	return reconcile.Result{}, nil
}

// createPersonaClusterRoles creates admin, edit, and view clusterroles that are
// namespace+stack+version specific
func (h *stackHandler) createPersonaClusterRoles(ctx context.Context, labels map[string]string) error {
	for persona := range roleVerbs {
		labelsCopy := map[string]string{}
		for k, v := range labels {
			labelsCopy[k] = v
		}

		rules := []rbacv1.PolicyRule{}
		for _, crd := range h.ext.Spec.CRDs {
			rules = append(rules, rbacv1.PolicyRule{
				APIGroups: []string{crd.GroupVersionKind().Group},
				Resources: []string{crd.Kind},
				Verbs:     roleVerbs[persona],
			})
		}
		name := stacks.PersonaRoleName(h.ext, persona)

		var crossplaneScope string

		if h.isNamespaced() {
			crossplaneScope = "namespace"
		} else {
			crossplaneScope = "environment"
		}

		aggregationLabel := fmt.Sprintf(stacks.LabelAggregateFmt, crossplaneScope, persona)

		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: labelsCopy,
			},
			Rules: rules,
		}

		meta.AddLabels(cr, map[string]string{
			aggregationLabel: "true",
		})

		meta.AddLabels(cr, stacks.ParentLabels(h.ext))

		if h.isNamespaced() {
			meta.AddLabels(cr, map[string]string{
				fmt.Sprintf(stacks.LabelNamespaceFmt, h.ext.GetNamespace()): "true",
			})
		}

		if err := h.kube.Create(ctx, cr); err != nil && !kerrors.IsAlreadyExists(err) {
			return errors.Wrap(err, "failed to create persona cluster roles")
		}
	}
	return nil
}

func generateNamespaceClusterRoles(stack *v1alpha1.Stack) (roles []*rbacv1.ClusterRole) {
	personas := []string{"admin", "edit", "view"}

	nsName := stack.GetNamespace()

	for _, persona := range personas {
		name := fmt.Sprintf(stacks.NamespaceClusterRoleNameFmt, nsName, persona)

		labels := map[string]string{
			fmt.Sprintf(stacks.LabelNamespaceFmt, nsName): "true",
			stacks.LabelScope: "namespace",
		}

		if persona == "admin" {
			labels[fmt.Sprintf(stacks.LabelAggregateFmt, "crossplane", persona)] = "true"
		}

		role := &rbacv1.ClusterRole{
			AggregationRule: &rbacv1.AggregationRule{
				ClusterRoleSelectors: []metav1.LabelSelector{
					{
						MatchLabels: map[string]string{
							fmt.Sprintf(stacks.LabelAggregateFmt, "namespace", persona): "true",
							fmt.Sprintf(stacks.LabelNamespaceFmt, nsName):               "true",
						},
					},
					{
						MatchLabels: map[string]string{
							fmt.Sprintf(stacks.LabelAggregateFmt, "namespace-default", persona): "true",
						},
					},
				},
			},

			// TODO(displague) set parent labels?
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: labels,
			},
		}

		roles = append(roles, role)
	}

	return roles
}

func (h *stackHandler) createNamespaceClusterRoles(ctx context.Context) error {
	if !h.isNamespaced() {
		return nil
	}

	ns := &corev1.Namespace{}
	nsName := h.ext.GetNamespace()

	if err := h.kube.Get(ctx, types.NamespacedName{Name: nsName}, ns); err != nil {
		return errors.Wrapf(err, "failed to get namespace %q for stackinstall %q", nsName, h.ext.GetName())
	}

	roles := generateNamespaceClusterRoles(h.ext)

	for _, role := range roles {
		role.SetOwnerReferences([]metav1.OwnerReference{
			{
				APIVersion: "v1",
				Kind:       "Namespace",
				Name:       nsName,
				UID:        ns.GetUID(),
			},
		})

		if err := h.kube.Create(ctx, role); err != nil && !kerrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "failed to create clusterrole %s for stackinstall %s", role.GetName(), h.ext.GetName())
		}
	}
	return nil
}

func (h *stackHandler) createDeploymentClusterRole(ctx context.Context, labels map[string]string) (string, error) {
	name := stacks.PersonaRoleName(h.ext, "system")
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Rules: h.ext.Spec.Permissions.Rules,
	}

	if err := h.kube.Create(ctx, cr); err != nil && !kerrors.IsAlreadyExists(err) {
		return "", errors.Wrap(err, "failed to create cluster role")
	}

	return name, nil
}

func (h *stackHandler) createNamespacedRoleBinding(ctx context.Context, clusterRoleName string, owner metav1.OwnerReference) error {
	// create rolebinding between service account and role
	crb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            h.ext.Name,
			Namespace:       h.ext.Namespace,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRoleName},
		Subjects: []rbacv1.Subject{
			{Name: h.ext.Name, Namespace: h.ext.Namespace, Kind: rbacv1.ServiceAccountKind},
		},
	}
	if err := h.kube.Create(ctx, crb); err != nil && !kerrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "failed to create role binding")
	}
	return nil
}

func (h *stackHandler) createClusterRoleBinding(ctx context.Context, clusterRoleName string, labels map[string]string) error {
	// create clusterrolebinding between service account and role
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   h.ext.Name,
			Labels: labels,
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRoleName},
		Subjects: []rbacv1.Subject{
			{Name: h.ext.Name, Namespace: h.ext.Namespace, Kind: rbacv1.ServiceAccountKind},
		},
	}
	if err := h.kube.Create(ctx, crb); err != nil && !kerrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "failed to create cluster role binding")
	}
	return nil
}

func (h *stackHandler) processRBAC(ctx context.Context) error {
	if len(h.ext.Spec.Permissions.Rules) == 0 {
		return nil
	}

	owner := meta.AsOwner(meta.ReferenceTo(h.ext, v1alpha1.StackGroupVersionKind))

	// create service account
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            h.ext.Name,
			Namespace:       h.ext.Namespace,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
	}

	if err := h.kube.Create(ctx, sa); err != nil && !kerrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "failed to create service account")
	}

	labels := stacks.ParentLabels(h.ext)

	clusterRoleName, err := h.createDeploymentClusterRole(ctx, labels)
	if err != nil {
		return err
	}

	// give the SA rolebindings to run the the stack's controller
	var roleBindingErr error

	switch apiextensions.ResourceScope(h.ext.Spec.PermissionScope) {
	case apiextensions.ClusterScoped:
		roleBindingErr = h.createClusterRoleBinding(ctx, clusterRoleName, labels)
	case "", apiextensions.NamespaceScoped:
		if nsClusterRoleErr := h.createNamespaceClusterRoles(ctx); nsClusterRoleErr != nil {
			return nsClusterRoleErr
		}
		roleBindingErr = h.createNamespacedRoleBinding(ctx, clusterRoleName, owner)

	default:
		roleBindingErr = errors.New("invalid permissionScope for stack")
	}

	if roleBindingErr != nil {
		return roleBindingErr
	}

	// create persona roles
	return h.createPersonaClusterRoles(ctx, labels)
}

func (h *stackHandler) isNamespaced() bool {
	return apiextensions.ResourceScope(h.ext.Spec.PermissionScope) == apiextensions.NamespaceScoped
}

func (h *stackHandler) processDeployment(ctx context.Context) error {
	controllerDeployment := h.ext.Spec.Controller.Deployment
	if controllerDeployment == nil {
		return nil
	}

	// ensure the deployment is set to use this stack's service account that we created
	deploymentSpec := *controllerDeployment.Spec.DeepCopy()
	deploymentSpec.Template.Spec.ServiceAccountName = h.ext.Name

	ref := meta.AsOwner(meta.ReferenceTo(h.ext, v1alpha1.StackGroupVersionKind))
	gvk := schema.GroupVersionKind{
		Group:   apps.GroupName,
		Kind:    "Deployment",
		Version: apps.SchemeGroupVersion.Version,
	}
	d := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            controllerDeployment.Name,
			Namespace:       h.ext.Namespace,
			OwnerReferences: []metav1.OwnerReference{ref},
		},
		Spec: deploymentSpec,
	}

	if err := h.kube.Create(ctx, d); err != nil && !kerrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "failed to create deployment")
	}

	// save a reference to the stack's controller
	h.ext.Status.ControllerRef = meta.ReferenceTo(d, gvk)

	return nil
}

func (h *stackHandler) processJob(ctx context.Context) error {
	controllerJob := h.ext.Spec.Controller.Job
	if controllerJob == nil {
		return nil
	}

	// ensure the job is set to use this stack's service account that we created
	jobSpec := *controllerJob.Spec.DeepCopy()
	jobSpec.Template.Spec.ServiceAccountName = h.ext.Name

	ref := meta.AsOwner(meta.ReferenceTo(h.ext, v1alpha1.StackGroupVersionKind))
	gvk := schema.GroupVersionKind{
		Group:   batch.GroupName,
		Kind:    "Job",
		Version: batch.SchemeGroupVersion.Version,
	}
	j := &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            controllerJob.Name,
			Namespace:       h.ext.Namespace,
			OwnerReferences: []metav1.OwnerReference{ref},
		},
		Spec: jobSpec,
	}
	if err := h.kube.Create(ctx, j); err != nil && !kerrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "failed to create job")
	}

	// save a reference to the stack's controller
	h.ext.Status.ControllerRef = meta.ReferenceTo(j, gvk)

	return nil
}

// delete performs clean up (finalizer) actions when a Stack is being deleted.
// This function ensures that all the resources (ClusterRoles, ClusterRoleBindings) that this StackInstall owns
// are also cleaned up.
func (h *stackHandler) delete(ctx context.Context) (reconcile.Result, error) {
	labels := stacks.ParentLabels(h.ext)

	crList := &rbacv1.ClusterRoleList{}

	if err := h.kube.List(ctx, crList, client.MatchingLabels(labels)); err != nil {
		return fail(ctx, h.kube, h.ext, err)
	}

	for i := range crList.Items {
		if err := h.kube.Delete(ctx, &crList.Items[i]); runtimeresource.IgnoreNotFound(err) != nil {
			return fail(ctx, h.kube, h.ext, err)
		}
	}

	crbList := &rbacv1.ClusterRoleBindingList{}

	if err := h.kube.List(ctx, crbList, client.MatchingLabels(labels)); err != nil {
		return fail(ctx, h.kube, h.ext, err)
	}

	for i := range crbList.Items {
		if err := h.kube.Delete(ctx, &crbList.Items[i]); runtimeresource.IgnoreNotFound(err) != nil {
			return fail(ctx, h.kube, h.ext, err)
		}
	}

	meta.RemoveFinalizer(h.ext, stacksFinalizer)
	if err := h.kube.Update(ctx, h.ext); err != nil {
		return fail(ctx, h.kube, h.ext, err)
	}

	return reconcile.Result{}, nil
}

// fail - helper function to set fail condition with reason and message
func fail(ctx context.Context, kube client.StatusClient, i *v1alpha1.Stack, err error) (reconcile.Result, error) {
	log.V(logging.Debug).Info("failed stack", "i", i.Name, "error", err)
	i.Status.SetConditions(runtimev1alpha1.ReconcileError(err))
	return resultRequeue, kube.Status().Update(ctx, i)
}
