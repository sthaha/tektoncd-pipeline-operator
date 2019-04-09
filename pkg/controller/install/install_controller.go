package install

import (
	"context"
	"flag"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/jcrossley3/manifestival/yaml"
	tektonv1alpha1 "github.com/openshift/tektoncd-pipeline-operator/pkg/apis/tekton/v1alpha1"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	filename    string
	autoInstall bool
	log         = logf.Log.WithName("controller_install")
)

var (
	ocp4workarounds string = filepath.Join("deploy", "resources", "ocp4hack.yaml")
)

func init() {
	flag.StringVar(&filename, "manifest", latestVersionDir("deploy/resources"),
		"The filename containing the tekton-cd pipeline release resources")
	flag.BoolVar(&autoInstall, "auto-install", false,
		"Automatically install pipeline if none exists")
}

// finds the directory in path that is latest or returns the path itself
func latestVersionDir(path string) string {
	entries, err := ioutil.ReadDir(path)
	if err != nil {
		return path
	}
	// find the latest dir traversing back
	if len(entries) == 0 {
		return path
	}

	latest := ""
	for i := len(entries) - 1; i >= 0; i-- {
		f := entries[i]
		if f.IsDir() {
			latest = filepath.Join(path, f.Name())
			break
		}
	}
	if latest == "" {
		return path
	}
	return latest
}

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new Install Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {

	return &ReconcileInstall{
		client:   mgr.GetClient(),
		scheme:   mgr.GetScheme(),
		manifest: yaml.NewYamlManifest(filename, mgr.GetConfig()),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("install-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Install
	err = c.Watch(&source.Kind{Type: &tektonv1alpha1.Install{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create that are owned by the primary resource
	// Watch for changes to secondary resource Pods and requeue the owner Install
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &tektonv1alpha1.Install{},
	})
	if err != nil {
		return err
	}

	if autoInstall {
		go createInstallCR(mgr.GetClient())
	}
	return nil
}

var _ reconcile.Reconciler = &ReconcileInstall{}

// ReconcileInstall reconciles a Install object
type ReconcileInstall struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client   client.Client
	scheme   *runtime.Scheme
	manifest *yaml.YamlManifest
}

func (r *ReconcileInstall) applyWorkarounds() (reconcile.Result, error) {
	logger := log.WithValues("workaround", "apply")

	res, err := decode(ocp4workarounds)
	if err != nil {
		logger.Error(err, "scc not found")
		return reconcile.Result{}, err
	}
	for _, obj := range res {
		gvk := obj.GroupVersionKind()
		logger.Info("creating", "gvk", gvk)

		if err := r.client.Create(context.TODO(), &obj); err != nil {
			logger.Error(err, "failed to apply workaround")
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileInstall) removeWorkarounds() (reconcile.Result, error) {
	logger := log.WithValues("workaround", "remove")

	res, err := decode(ocp4workarounds)
	if err != nil {
		logger.Error(err, "scc not found")
		return reconcile.Result{}, err
	}
	for _, obj := range res {
		gvk := obj.GroupVersionKind()
		logger.Info("removing", "gvk", gvk)

		if err := r.client.Delete(context.TODO(), &obj); err != nil {
			logger.Error(err, "failed to remove workaround")
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

// Reconcile reads that state of the cluster for a Install object and makes changes based on the state read
// and what is in the Install.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileInstall) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Install")
	reqLogger.Info("Pipeline Release Path", "path", filename)

	// Fetch the Install instance
	instance := &tektonv1alpha1.Install{}

	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			if r, err := r.removeWorkarounds(); err != nil {
				return r, err
			}
			return reconcile.Result{}, r.manifest.Delete()
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if instance.Status.Resources != nil {
		reqLogger.Info("skipping installation resources already set for setup crd")
		return reconcile.Result{}, nil
	}
	if res, err := r.applyWorkarounds(); err != nil {
		reqLogger.Error(err, "failed to apply workarounds")
		return res, err
	}

	if err := r.manifest.Apply(instance); err != nil {
		reqLogger.Error(err, "failed to apply pipeline manifest")
		return reconcile.Result{}, err
	}

	// Update status
	instance.Status.Resources = r.manifest.ResourceNames()
	instance.Status.Version = "HACK" // figure out the version
	if err = r.client.Status().Update(context.TODO(), instance); err != nil {
		reqLogger.Error(err, "Failed to update status")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil

}

func createInstallCR(c client.Client) error {
	installLog := log.WithValues("sub", "auto-install")

	ns, err := k8sutil.GetWatchNamespace()
	if err != nil {
		return err
	}

	installList := &tektonv1alpha1.InstallList{}
	err = c.List(context.TODO(), &client.ListOptions{Namespace: ns}, installList)
	if err != nil {
		installLog.Error(err, "Unable to list Installs")
		return err
	}
	if len(installList.Items) >= 1 {
		return nil
	}

	install := &tektonv1alpha1.Install{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "auto-install",
			Namespace: ns,
		},
	}
	if err := c.Create(context.TODO(), install); err != nil {
		installLog.Error(err, "auto-install: failed to create Install CR")
		return err
	}
	return nil
}

func decode(path string) ([]unstructured.Unstructured, error) {
	result := []unstructured.Unstructured{}

	r, err := os.Open(path)
	if err != nil {
		log.Error(err, "failed to open", "path", path)
		return result, err
	}
	defer r.Close()

	doc := yamlutil.NewYAMLToJSONDecoder(r)

	for {
		res := unstructured.Unstructured{}
		if err = doc.Decode(&res); err != nil {
			if err == io.EOF {
				break
			}
			return result, err
		}

		result = append(result, res)
	}
	return result, nil
}
