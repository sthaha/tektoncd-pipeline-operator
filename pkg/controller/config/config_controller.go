package config

import (
	"context"
	"flag"
	"path/filepath"

	"github.com/go-logr/logr"
	mf "github.com/jcrossley3/manifestival"
	op "github.com/openshift/tektoncd-pipeline-operator/pkg/apis/operator/v1alpha1"
	"github.com/operator-framework/operator-sdk/pkg/predicate"
	"github.com/prometheus/common/log"
	appsv1 "k8s.io/api/apps/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	ClusterCRName   = "cluster"
	DefaultTargetNs = "openshift-pipelines"

	// Name of the pipeline controller deployment
	PipelineControllerName = "tekton-pipelines-controller"

	// Name of the pipeline webhook deployment
	PipelineWebhookName = "tekton-pipelines-webhook"
)

var (
	tektonVersion   = "v0.4.0"
	resourceWatched string
	resourceDir     string
	targetNamespace string
	noAutoInstall   bool
	recursive       bool
	ctrlLog         = logf.Log.WithName("ctrl").WithName("config")
)

func init() {
	flag.StringVar(
		&resourceWatched, "watch-resource", ClusterCRName,
		"cluster-wide resource that operator honours, default: "+ClusterCRName)

	flag.StringVar(
		&targetNamespace, "target-namespace", DefaultTargetNs,
		"Namespace where pipeline will be installed default: "+DefaultTargetNs)

	defaultResDir := filepath.Join("deploy", "resources", tektonVersion)
	flag.StringVar(
		&resourceDir, "resource-dir", defaultResDir,
		"Path to resource manifests, default: "+defaultResDir)

	flag.BoolVar(
		&noAutoInstall, "no-auto-install", false,
		"Do not automatically install tekton pipelines, default: false")

	flag.BoolVar(
		&recursive, "recursive", false,
		"If enabled apply manifest file in resource directory recursively")

	ctrlLog.Info("configuration",
		"resource-watched", resourceWatched,
		"targetNamespace", targetNamespace,
		"no-auto-install", noAutoInstall,
	)
}

// Add creates a new Config Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	m, err := mf.NewManifest(resourceDir, recursive, mgr.GetClient())
	if err != nil {
		return err
	}
	return add(mgr, newReconciler(mgr, m))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, m mf.Manifest) reconcile.Reconciler {
	return &ReconcileConfig{
		client:   mgr.GetClient(),
		scheme:   mgr.GetScheme(),
		manifest: m,
		addons:   make(map[string]mf.Manifest),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	log := ctrlLog.WithName("add")
	// Create a new controller
	c, err := controller.New("config-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Config
	log.Info("Watching operator config CR")
	err = c.Watch(
		&source.Kind{Type: &op.Config{}},
		&handler.EnqueueRequestForObject{},
		predicate.GenerationChangedPredicate{},
	)
	if err != nil {
		return err
	}

	err = c.Watch(
		&source.Kind{Type: &appsv1.Deployment{}},
		&handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &op.Config{},
		})
	if err != nil {
		return err
	}

	if noAutoInstall {
		return nil
	}

	if err := createCR(mgr.GetClient()); err != nil {
		log.Error(err, "creation of config resource failed")
		return err
	}
	return nil
}

// blank assignment to verify that ReconcileConfig implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileConfig{}

// ReconcileConfig reconciles a Config object
type ReconcileConfig struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client   client.Client
	scheme   *runtime.Scheme
	manifest mf.Manifest
	addons   map[string]mf.Manifest
}

// Reconcile reads that state of the cluster for a Config object and makes changes based on the state read
// and what is in the Config.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileConfig) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	log := requestLogger(req, "reconcile")

	log.Info("reconciling config change")

	if ignoreRequest(req) {
		log.Info("ignoring event of resource watched that is not cluster-wide")
		return reconcile.Result{}, nil
	}

	cfg := &op.Config{}
	err := r.client.Get(context.TODO(), req.NamespacedName, cfg)

	// ignore all resources except the `resourceWatched`
	if req.Name != resourceWatched {
		log.Info("ignoring incorrect object")

		// handle resources that are not interesting as error
		if !errors.IsNotFound(err) {
			r.markInvalidResource(cfg)
		}
		return reconcile.Result{}, nil
	}

	// handle deletion of resource
	if errors.IsNotFound(err) {
		// User deleted the cluster resource so delete the pipeine resources
		log.Info("resource has been deleted")
		return r.reconcileDeletion(req, cfg)
	}

	// Error reading the object - requeue the request.
	if err != nil {
		log.Error(err, "requeueing event since there was an error reading object")
		return reconcile.Result{}, err
	}

	if res, err := r.reconcilePipeline(req, cfg); err != nil {
		return res, err
	}

	return r.reconcileAddons(req, cfg)

	//////////////////////////////////////////////////////////////////////

	// out of n
	err = r.deleteRemovedAddons(req, cfg)
	if err != nil {
		log.Error(err, "failed to delete removed addon")
		return reconcile.Result{}, err
	}

	if isUpToDate(cfg) {
		log.Info("skipping installation, resource already up to date")

		// Cheking for new addons
		tfs := []mf.Transformer{
			mf.InjectOwner(cfg),
			mf.InjectNamespace(cfg.Spec.TargetNamespace),
		}
		if err := r.installAddons(req, cfg, tfs); err != nil {
			log.Error(err, "failed to install addons")
			// ignoring failure to update
			_ = r.updateStatus(cfg, op.ConfigCondition{
				Code:    op.ErrorStatus,
				Details: err.Error(),
				Version: tektonVersion})
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	log.Info("installing pipelines", "path", resourceDir)

	return r.reconcileInstall(req, cfg)

}
func addonsChanged(cfg op.Config) bool {
	addons := cfg.Spec.Addons
	status := cfg.Status.Addons

	// new resource and the status is empty so only check if there
	// are any addons to install
	if len(status) == 0 {
		return len(addons) != 0
	}

	// operator has a status so it may have installaled some addons
	// diff addons and lastStatus.resources
	// []string{} == []string{}

	//lastStatus = status[0]
	//if len(addons) > 0 && len(stau)

}

type Mods struct {
	Added, Updated, Deleted []string

func (r *ReconcileConfig) reconcileAddons(req reconcile.Request, cfg *op.Config) (reconcile.Result, error) {
	mods := diffAddons(config)
	if !mods.any() {
		return reconcile.Result{}, nil

	}
	installAddons(mods.Added)
	deleteAddons(mods.Deleted)
	updateAddons(mods.Updated)
	return reconcile.Result{}, nil
}

func (r *ReconcileConfig) reconcilePipeline(req reconcile.Request, cfg *op.Config) (reconcile.Result, error) {
	log := requestLogger(req, "pipeline")
	if isUpToDate(cfg) {
		log.Info("skipping installation, resource already up to date")
		return reconcile.Result{}, nil
	}

	err := r.updateStatus(cfg, op.ConfigCondition{Code: op.InstallingStatus, Version: tektonVersion})
	if err != nil {
		log.Error(err, "failed to set status")
		return reconcile.Result{}, err
	}

	tfs := []mf.Transformer{
		mf.InjectOwner(cfg),
		mf.InjectNamespace(cfg.Spec.TargetNamespace),
	}

	if err := r.manifest.Transform(tfs...); err != nil {
		log.Error(err, "failed to apply manifest transformations")
		// ignoring failure to update
		_ = r.updateStatus(cfg, op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: err.Error(),
			Version: tektonVersion})
		return reconcile.Result{}, err
	}

	if err := r.manifest.ApplyAll(); err != nil {
		log.Error(err, "failed to apply release.yaml")
		// ignoring failure to update
		_ = r.updateStatus(cfg, op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: err.Error(),
			Version: tektonVersion})
		return reconcile.Result{}, err
	}

	log.Info("successfully applied all resources")

	// NOTE: manifest when updating (not installing) already installed resources
	// modifies the `cfg` but does not refersh it, hence refresh manually
	if err := r.refreshCR(cfg); err != nil {
		log.Error(err, "status update failed to refresh object")
		return reconcile.Result{}, err
	}

	err = r.updateStatus(cfg, op.ConfigCondition{
		Code: op.InstalledStatus, Version: tektonVersion})
	return reconcile.Result{}, err
}

func (r *ReconcileConfig) reconcileInstall(req reconcile.Request, cfg *op.Config) (reconcile.Result, error) {
	log := requestLogger(req, "install")

	err := r.updateStatus(cfg, op.ConfigCondition{Code: op.InstallingStatus, Version: tektonVersion})
	if err != nil {
		log.Error(err, "failed to set status")
		return reconcile.Result{}, err
	}

	tfs := []mf.Transformer{
		mf.InjectOwner(cfg),
		mf.InjectNamespace(cfg.Spec.TargetNamespace),
	}

	if err := r.manifest.Transform(tfs...); err != nil {
		log.Error(err, "failed to apply manifest transformations")
		// ignoring failure to update
		_ = r.updateStatus(cfg, op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: err.Error(),
			Version: tektonVersion})
		return reconcile.Result{}, err
	}

	if err := r.manifest.ApplyAll(); err != nil {
		log.Error(err, "failed to apply release.yaml")
		// ignoring failure to update
		_ = r.updateStatus(cfg, op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: err.Error(),
			Version: tektonVersion})
		return reconcile.Result{}, err
	}

	if err := r.installAddons(req, cfg, tfs); err != nil {
		log.Error(err, "failed to install addons")
		// ignoring failure to update
		_ = r.updateStatus(cfg, op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: err.Error(),
			Version: tektonVersion})
		return reconcile.Result{}, err
	}

	log.Info("successfully applied all resources")

	// NOTE: manifest when updating (not installing) already installed resources
	// modifies the `cfg` but does not refersh it, hence refresh manually
	if err := r.refreshCR(cfg); err != nil {
		log.Error(err, "status update failed to refresh object")
		return reconcile.Result{}, err
	}

	err = r.updateStatus(cfg, op.ConfigCondition{
		Code: op.InstalledStatus, Version: tektonVersion})
	return reconcile.Result{}, err
}

func (r *ReconcileConfig) installAddons(req reconcile.Request, cfg *op.Config, tfs []mf.Transformer) error {
	log := requestLogger(req, "install addons")

	for _, path := range cfg.Spec.AddOns {
		_, ok := r.addons[path]
		if !ok {
			extension, err := mf.NewManifest(filepath.Join("deploy", "resources", path), true, r.client)
			if err != nil {
				log.Error(err, "failed to read addon  manifest")
				// ignoring failure to update
				_ = r.updateStatus(cfg, op.ConfigCondition{
					Code:    op.ErrorStatus,
					Details: err.Error(),
					Version: tektonVersion})
				return err
			}
			if err = extension.Transform(tfs...); err != nil {
				log.Error(err, "failed to apply manifest transformations")
				// ignoring failure to update
				_ = r.updateStatus(cfg, op.ConfigCondition{
					Code:    op.ErrorStatus,
					Details: err.Error(),
					Version: tektonVersion})
				return err
			}

			if err = extension.ApplyAll(); err != nil {
				log.Error(err, "failed to apply release.yaml")
				// ignoring failure to update
				_ = r.updateStatus(cfg, op.ConfigCondition{
					Code:    op.ErrorStatus,
					Details: err.Error(),
					Version: tektonVersion})
				return err
			}
			r.addons[path] = extension
		}
	}

	log.Info("successfully installed all addons")
	return nil
}

func (r *ReconcileConfig) reconcileDeletion(req reconcile.Request, cfg *op.Config) (reconcile.Result, error) {
	log := requestLogger(req, "delete")

	log.Info("deleting pipeline resources")

	// Requested object not found, could have been deleted after reconcile request.
	// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
	propPolicy := client.PropagationPolicy(metav1.DeletePropagationForeground)

	if err := r.manifest.DeleteAll(propPolicy); err != nil {
		log.Error(err, "failed to delete pipeline resources")
		return reconcile.Result{}, err
	}

	// Return and don't requeue
	return reconcile.Result{}, nil

}

func (r *ReconcileConfig) deleteRemovedAddons(req reconcile.Request, cfg *op.Config) error {
	log := requestLogger(req, "delete addons")

	log.Info("checking removed addons ")
	for path, extension := range r.addons {
		log.Info("checking addon: " + path)
		exist := false
		for _, entry := range cfg.Spec.AddOns {
			if entry == path {
				exist = true
			}
		}
		if !exist {
			log.Info("deleting addon: " + path)
			delete(r.addons, path)

			// Requested object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			propPolicy := client.PropagationPolicy(metav1.DeletePropagationForeground)

			if err := extension.DeleteAll(propPolicy); err != nil {
				log.Error(err, "failed to delete addon")
				// ignoring failure to update
				_ = r.updateStatus(cfg, op.ConfigCondition{
					Code:    op.ErrorStatus,
					Details: err.Error(),
					Version: tektonVersion})
				return err
			}
		}
	}
	return nil
}

// markInvalidResource sets the status of resourse as invalid
func (r *ReconcileConfig) markInvalidResource(cfg *op.Config) {
	err := r.updateStatus(cfg,
		op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: "metadata.name must be " + resourceWatched,
			Version: "unknown"})
	if err != nil {
		ctrlLog.Info("failed to update status as invalid")
	}
}

// updateStatus set the status of cfg to s and refreshes cfg to the lastest version
func (r *ReconcileConfig) updateStatus(cfg *op.Config, c op.ConfigCondition) error {

	// NOTE: need to use a deepcopy since Status().Update() seems to reset the
	// APIVersion of the cfg to "" making the object invalid; may be a mechanism
	// to prevent us from using stale version of the object

	mods := cfg.DeepCopy()
	mods.Status.Conditions = append([]op.ConfigCondition{c}, mods.Status.Conditions...)
	return r.updateConfig(cfg, mods)
}

// updateAddonsStatus set the status of cfg to c and refreshes cfg to the lastest version
func (r *ReconcileConfig) updateAddonsStatus(cfg *op.Config, c op.AddonsCondition) error {
	// NOTE: need to use a deepcopy since Status().Update() seems to reset the
	// APIVersion of the cfg to "" making the object invalid; may be a mechanism
	// to prevent us from using stale version of the object

	mods := cfg.DeepCopy()
	mods.Status.Addons = append([]op.AddonsCondition{c}, mods.Status.Addons...)
	return r.updateConfig(cfg, mods)
}

// updateConfig applies the mods to the config CR and refreshes it
func (r *ReconcileConfig) updateConfig(cfg *op.Config, mods *op.Config) error {
	if err := r.client.Status().Update(context.TODO(), mods); err != nil {
		log.Error(err, "status update failed")
		return err
	}

	if err := r.refreshCR(cfg); err != nil {
		log.Error(err, "status update failed to refresh object")
		return err
	}
	return nil
}

func (r *ReconcileConfig) refreshCR(cfg *op.Config) error {
	objKey := types.NamespacedName{
		Namespace: cfg.Namespace,
		Name:      cfg.Name,
	}
	return r.client.Get(context.TODO(), objKey, cfg)
}

func createCR(c client.Client) error {
	log := ctrlLog.WithName("create-cr").WithValues("name", resourceWatched)
	log.Info("creating a clusterwide resource of config crd")

	cr := &op.Config{
		ObjectMeta: metav1.ObjectMeta{Name: resourceWatched},
		Spec:       op.ConfigSpec{TargetNamespace: targetNamespace},
	}

	err := c.Create(context.TODO(), cr)
	if errors.IsAlreadyExists(err) {
		log.Info("skipped creation", "reason", "resoure already exists")
		return nil
	}

	return err
}

func isUpToDate(r *op.Config) bool {
	c := r.Status.Conditions
	if len(c) == 0 {
		return false
	}

	latest := c[0]
	return latest.Version == tektonVersion &&
		latest.Code == op.InstalledStatus
}

func requestLogger(req reconcile.Request, context string) logr.Logger {
	return ctrlLog.WithName(context).WithValues(
		"Request.Namespace", req.Namespace,
		"Request.NamespaceName", req.NamespacedName,
		"Request.Name", req.Name)
}

func ignoreRequest(req reconcile.Request) bool {
	// NOTE: only interested in the clusterwide events of the resource watched
	// Otherwise, when release.yaml is applied and onwer-ref is set to the
	// clusterwide resource, events are generated for "targetNamespace/resourceWatched"
	// and as the GET call for that resource fails, the pipeline resources get
	// deleted. this filter prevents that from happening
	return req.Name == resourceWatched && req.Namespace != ""
}
