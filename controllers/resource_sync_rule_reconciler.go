// Copyright (c) 2021 Banzai Cloud Zrt. All Rights Reserved.

package controllers

import (
	"context"
	"reflect"

	"emperror.dev/errors"
	"github.com/go-logr/logr"
	"github.com/throttled/throttled"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/banzaicloud/cluster-registry-controller/internal/config"
	"github.com/banzaicloud/cluster-registry-controller/pkg/clusters"
	"github.com/banzaicloud/cluster-registry-controller/pkg/ratelimit"
	clusterregistryv1alpha1 "github.com/banzaicloud/cluster-registry/api/v1alpha1"
)

type SyncReconciler interface {
	clusters.ManagedReconciler

	GetRule() *clusterregistryv1alpha1.ResourceSyncRule
}

type ResourceSyncRuleReconciler struct {
	clusters.ManagedReconciler

	clustersManager *clusters.Manager
	config          config.Configuration

	queue workqueue.RateLimitingInterface
}

func NewResourceSyncRuleReconciler(name string, log logr.Logger, clustersManager *clusters.Manager, config config.Configuration) *ResourceSyncRuleReconciler {
	return &ResourceSyncRuleReconciler{
		ManagedReconciler: clusters.NewManagedReconciler(name, log),

		clustersManager: clustersManager,
		config:          config,
	}
}

func (r *ResourceSyncRuleReconciler) setQueue(q workqueue.RateLimitingInterface) {
	r.queue = q
}

func (r *ResourceSyncRuleReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	log := r.GetLogger().WithValues("rule", req.NamespacedName)

	result, err := r.reconcile(req, log)
	if err != nil {
		//nolint:errorlint
		if e, ok := err.(interface{ IsPermanent() bool }); ok && e.IsPermanent() {
			log.Error(err, "", errors.GetDetails(err)...)
			err = nil
		}
	}

	return result, errors.WithStackIf(err)
}

func (r *ResourceSyncRuleReconciler) reconcile(req ctrl.Request, log logr.Logger) (ctrl.Result, error) {
	log.Info("reconciling")

	sr := &clusterregistryv1alpha1.ResourceSyncRule{}
	err := r.GetManager().GetClient().Get(r.GetContext(), req.NamespacedName, sr)
	if apierrors.IsNotFound(err) {
		for _, cluster := range r.clustersManager.GetAll() {
			cluster.RemoveControllerByName(req.NamespacedName.Name)
		}

		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, cluster := range r.clustersManager.GetAll() {
		log.Info("sync controller", "ctrl", sr.Name, "cluster", cluster.GetName())
		err := r.syncClusterController(cluster, sr)
		if err != nil {
			r.GetLogger().Error(err, "could not sync controller")
		}
	}

	return ctrl.Result{}, nil
}

func (r *ResourceSyncRuleReconciler) syncClusterController(cluster *clusters.Cluster, sr *clusterregistryv1alpha1.ResourceSyncRule) error {
	var ctrl clusters.ManagedController
	var err error

	if !cluster.HasController(sr.Name) {
		_, err = InitNewResourceSyncController(sr, cluster, r.clustersManager, r.GetManager(), r.GetLogger(), r.config)
		if err != nil {
			return err
		}

		return nil
	}

	ctrl = cluster.GetController(sr.Name)

	actualRule := &clusterregistryv1alpha1.ResourceSyncRule{}
	if rec, ok := ctrl.GetReconciler().(SyncReconciler); ok {
		actualRule = rec.GetRule()
	}

	if actualRule != nil && !reflect.DeepEqual(actualRule.Spec, sr.Spec) {
		r.GetLogger().Info("needs regenerate")
		cluster.RemoveController(ctrl)
		<-ctrl.Stopped()
		_, err = InitNewResourceSyncController(sr, cluster, r.clustersManager, r.GetManager(), r.GetLogger(), r.config)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *ResourceSyncRuleReconciler) SetupWithController(ctx context.Context, ctrl controller.Controller) error {
	err := r.ManagedReconciler.SetupWithController(ctx, ctrl)
	if err != nil {
		return err
	}

	err = ctrl.Watch(&InMemorySource{
		reconciler: r,
	}, handler.Funcs{})
	if err != nil {
		return err
	}

	r.clustersManager.AddOnAfterAddFunc(func(c *clusters.Cluster) {
		if r.queue != nil {
			rules := &clusterregistryv1alpha1.ResourceSyncRuleList{}
			err := r.GetManager().GetClient().List(r.GetContext(), rules)
			if err != nil {
				r.GetLogger().Error(err, "could not list resource sync rules")
			}
			for _, rule := range rules.Items {
				r.queue.Add(reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name: rule.Name,
					},
				})
			}
		}
	}, "trigger-resource-sync-rule-reconcile")

	return nil
}

func (r *ResourceSyncRuleReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	err := r.ManagedReconciler.SetupWithManager(ctx, mgr)
	if err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr)

	ctrl, err := b.For(&clusterregistryv1alpha1.ResourceSyncRule{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ResourceSyncRule",
			APIVersion: clusterregistryv1alpha1.SchemeBuilder.GroupVersion.String(),
		},
	}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.config.SyncController.WorkerCount,
		}).
		Build(r)
	if err != nil {
		return err
	}

	err = r.SetupWithController(ctx, ctrl)
	if err != nil {
		return err
	}

	return nil
}

func InitNewResourceSyncController(rule *clusterregistryv1alpha1.ResourceSyncRule, cluster *clusters.Cluster, clustersManager *clusters.Manager, mgr ctrl.Manager, log logr.Logger, config config.Configuration) (clusters.ManagedController, error) {
	rl, err := ratelimit.NewRateLimiter(config.SyncController.RateLimit.MaxKeys, &throttled.RateQuota{
		MaxRate:  throttled.PerSec(config.SyncController.RateLimit.MaxRatePerSecond),
		MaxBurst: config.SyncController.RateLimit.MaxBurst,
	})
	if err != nil {
		return nil, errors.WrapIf(err, "could not create rate limiter")
	}

	log = log.WithName(rule.Name)
	srec, err := NewSyncReconciler(rule.Name, mgr, rule, log, cluster.GetClusterID(), clustersManager, WithRateLimiter(rl))
	if err != nil {
		return nil, errors.WithStackIf(err)
	}
	ctrl := clusters.NewManagedController(rule.Name, srec, log)

	return ctrl, cluster.AddController(ctrl)
}