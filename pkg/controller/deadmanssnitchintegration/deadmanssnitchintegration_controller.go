package deadmanssnitchintegration

import (
	"context"
	"fmt"

	"github.com/openshift/deadmanssnitch-operator/config"
	deadmanssnitchv1alpha1 "github.com/openshift/deadmanssnitch-operator/pkg/apis/deadmanssnitch/v1alpha1"
	"github.com/openshift/deadmanssnitch-operator/pkg/dmsclient"
	"github.com/openshift/deadmanssnitch-operator/pkg/localmetrics"
	"github.com/openshift/deadmanssnitch-operator/pkg/utils"
	hivev1 "github.com/openshift/hive/apis/hive/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_deadmanssnitchintegration")

const (
	deadMansSnitchAPISecretKey    = "deadmanssnitch-api-key"
	DeadMansSnitchFinalizerPrefix = "dms.managed.openshift.io/deadmanssnitch-"
	// This can be removed once Hive is promoted past f73ed3e in all environments
	// Support for this condition was removed in https://github.com/openshift/hive/pull/1604
	legacyHivev1RunningHibernationReason = "Running"
)

// Add creates a new DeadmansSnitchIntegration Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileDeadmansSnitchIntegration{
		//client:    mgr.GetClient(),
		client:    mgr.GetClient(),
		scheme:    mgr.GetScheme(),
		dmsclient: dmsclient.NewClient,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("deadmanssnitchintegration-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource DeadmansSnitchIntegration
	err = c.Watch(&source.Kind{Type: &deadmanssnitchv1alpha1.DeadmansSnitchIntegration{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &hivev1.ClusterDeployment{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: clusterDeploymentToDeadMansSnitchIntegrationsMapper{
				Client: mgr.GetClient(),
			},
		},
	)
	if err != nil {
		return err
	}

	// Watch for changes to SyncSets. If one has any ClusterDeployment owner
	// references, queue a request for all DeadMansSnitchIntegration CR that
	// select those ClusterDeployments.
	err = c.Watch(&source.Kind{Type: &hivev1.SyncSet{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: ownedByClusterDeploymentToDeadMansSnitchIntegrationsMapper{
				Client: mgr.GetClient(),
			},
		},
	)
	if err != nil {
		return err
	}

	// Watch for changes to Secrets. If one has any ClusterDeployment owner
	// references, queue a request for all DeadMansSnitchIntegration CR that
	// select those ClusterDeployments.
	err = c.Watch(&source.Kind{Type: &corev1.Secret{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: ownedByClusterDeploymentToDeadMansSnitchIntegrationsMapper{
				Client: mgr.GetClient(),
			},
		},
	)
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileDeadmansSnitchIntegration implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileDeadmansSnitchIntegration{}

// ReconcileDeadmansSnitchIntegration reconciles a DeadmansSnitchIntegration object
type ReconcileDeadmansSnitchIntegration struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client    client.Client
	scheme    *runtime.Scheme
	dmsclient func(authToken string, collector *localmetrics.MetricsCollector) dmsclient.Client
}

// Reconcile reads that state of the cluster for a DeadmansSnitchIntegration object and makes changes based on the state read
// and what is in the DeadmansSnitchIntegration.Spec
func (r *ReconcileDeadmansSnitchIntegration) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling DeadmansSnitchIntegration")

	if config.IsFedramp() {
		reqLogger.Info("Running in FedRAMP mode")
	}

	// Fetch the DeadmansSnitchIntegration dmsi
	dmsi := &deadmanssnitchv1alpha1.DeadmansSnitchIntegration{}

	err := r.client.Get(context.TODO(), request.NamespacedName, dmsi)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// set the DMS finalizer variable
	deadMansSnitchFinalizer := DeadMansSnitchFinalizerPrefix + dmsi.Name

	dmsAPIKey, err := utils.LoadSecretData(r.client, dmsi.Spec.DmsAPIKeySecretRef.Name,
		dmsi.Spec.DmsAPIKeySecretRef.Namespace, deadMansSnitchAPISecretKey)
	if err != nil {
		return reconcile.Result{}, err
	}
	dmsc := r.dmsclient(dmsAPIKey, localmetrics.Collector)

	matchingClusterDeployments, err := r.getMatchingClusterDeployment(dmsi)
	if err != nil {
		return reconcile.Result{}, err
	}

	allClusterDeployments, err := r.getAllClusterDeployment()
	if err != nil {
		return reconcile.Result{}, err
	}

	if dmsi.DeletionTimestamp != nil {
		for _, clusterdeployment := range allClusterDeployments.Items {
			if utils.HasFinalizer(&clusterdeployment, deadMansSnitchFinalizer) {
				err = r.deleteDMSClusterDeployment(dmsi, &clusterdeployment, dmsc)
				if err != nil {
					return reconcile.Result{}, err
				}
			}
		}
		if utils.HasFinalizer(dmsi, deadMansSnitchFinalizer) {
			utils.DeleteFinalizer(dmsi, deadMansSnitchFinalizer)
			reqLogger.Info("Deleting DMSI finalizer from dmsi", "DeadMansSnitchIntegration.Namespace", dmsi.Namespace, "DMSI.Name", dmsi.Name)
			err = r.client.Update(context.TODO(), dmsi)
			if err != nil {
				reqLogger.Error(err, "Error deleting Finalizer from dmsi")
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	for _, clusterdeployment := range allClusterDeployments.Items {

		// Check if the cluster matches the requirements for needing DMS setup
		clusterMatched := false
		for _, matchingClusterDeployment := range matchingClusterDeployments {
			if clusterdeployment.UID == matchingClusterDeployment.UID {
				clusterMatched = true
				break
			}
		}

		if !clusterMatched || clusterdeployment.DeletionTimestamp != nil {
			// The cluster does not match the criteria for needing DMS setup
			if utils.HasFinalizer(&clusterdeployment, deadMansSnitchFinalizer) {
				// The cluster has an existing DMS setup, so remove it
				err = r.deleteDMSClusterDeployment(dmsi, &clusterdeployment, dmsc)
				if err != nil {
					return reconcile.Result{}, err
				}
			}
			continue
		}

		if !clusterdeployment.Spec.Installed {
			// The cluster isn't installed yet, so don't setup DMS yet either
			continue
		}

		err = r.dmsAddFinalizer(dmsi, &clusterdeployment)
		if err != nil {
			return reconcile.Result{}, err
		}

		secretExist, syncSetExist, err := r.snitchResourcesExist(dmsi, &clusterdeployment)
		if err != nil {
			return reconcile.Result{}, err
		}
		specIsHibernating := clusterdeployment.Spec.PowerState == hivev1.HibernatingClusterPowerState

		if specIsHibernating {
			if secretExist || syncSetExist {
				err := r.deleteDMSClusterDeployment(dmsi, &clusterdeployment, dmsc)
				if err != nil {
					return reconcile.Result{}, err
				}
			}
		} else {
			if instancesAreRunning(clusterdeployment) {
				if !secretExist || !syncSetExist {
					err = r.createSnitch(dmsi, &clusterdeployment, dmsc)
					if err != nil {
						return reconcile.Result{}, err
					}

					err = r.createSecret(dmsi, dmsc, clusterdeployment)
					if err != nil {
						return reconcile.Result{}, err
					}

					err = r.createSyncset(dmsi, clusterdeployment)
					if err != nil {
						return reconcile.Result{}, err
					}
				}
			}
		}
	}

	log.Info("Reconcile of deadmanssnitch integration complete")

	return reconcile.Result{}, nil
}

// getMatchingClusterDeployment gets all ClusterDeployments matching the DMSI selector
func (r *ReconcileDeadmansSnitchIntegration) getMatchingClusterDeployment(dmsi *deadmanssnitchv1alpha1.DeadmansSnitchIntegration) ([]hivev1.ClusterDeployment, error) {
	selector, err := metav1.LabelSelectorAsSelector(&dmsi.Spec.ClusterDeploymentSelector)
	if err != nil {
		return nil, err
	}

	matchingClusterDeployments := &hivev1.ClusterDeploymentList{}
	listOpts := &client.ListOptions{LabelSelector: selector}
	err = r.client.List(context.TODO(), matchingClusterDeployments, listOpts)

	matchedClusterDeployments := []hivev1.ClusterDeployment{}

	// If the ClusterDeploymentAnnotationsToSkip set in the DMS integration
	// Check the cluster deployment and skip it if the annotation has the same
	// key and value
	if len(dmsi.Spec.ClusterDeploymentAnnotationsToSkip) > 0 {
		for _, cd := range matchingClusterDeployments.Items {
			if !r.shouldSkipClusterDeployment(dmsi.Spec.ClusterDeploymentAnnotationsToSkip, cd) {
				matchedClusterDeployments = append(matchedClusterDeployments, cd)
			}
		}
	} else {
		matchedClusterDeployments = matchingClusterDeployments.Items
	}

	return matchedClusterDeployments, err
}

func (r *ReconcileDeadmansSnitchIntegration) shouldSkipClusterDeployment(clusterDeploymentAnnotationsToSkip []deadmanssnitchv1alpha1.ClusterDeploymentAnnotationsToSkip, cd hivev1.ClusterDeployment) bool {
	for annoKey, annoVal := range cd.GetAnnotations() {
		for _, skipper := range clusterDeploymentAnnotationsToSkip {
			if annoKey == skipper.Name && annoVal == skipper.Value {
				return true
			}
		}
	}
	return false
}

// getAllClusterDeployment retrives all ClusterDeployments in the shard
func (r *ReconcileDeadmansSnitchIntegration) getAllClusterDeployment() (*hivev1.ClusterDeploymentList, error) {
	matchingClusterDeployments := &hivev1.ClusterDeploymentList{}
	err := r.client.List(context.TODO(), matchingClusterDeployments, &client.ListOptions{})
	return matchingClusterDeployments, err
}

// Add finalizers to both the deadmanssnitch integration and the matching cluster deployment
func (r *ReconcileDeadmansSnitchIntegration) dmsAddFinalizer(dmsi *deadmanssnitchv1alpha1.DeadmansSnitchIntegration, clusterdeployment *hivev1.ClusterDeployment) error {
	deadMansSnitchFinalizer := DeadMansSnitchFinalizerPrefix + dmsi.Name
	logger := log.WithValues("DeadMansSnitchIntegration.Namespace", dmsi.Namespace, "DMSI.Name", dmsi.Name, "cluster-deployment.Name:", clusterdeployment.Name, "cluster-deployment.Namespace:", clusterdeployment.Namespace)
	//checking i finalizers exits in the clusterdeployment adding if they dont
	logger.Info("Checking for finalizers")
	if !utils.HasFinalizer(clusterdeployment, deadMansSnitchFinalizer) {
		log.Info(fmt.Sprint("Adding finalizer to cluster Deployment Name:  ", clusterdeployment.Name+" namespace:"+clusterdeployment.Namespace+" DMSI Name  :"+dmsi.Name))
		baseToPatch := client.MergeFrom(clusterdeployment.DeepCopy())
		utils.AddFinalizer(clusterdeployment, deadMansSnitchFinalizer)
		if err := r.client.Patch(context.TODO(), clusterdeployment, baseToPatch); err != nil {
			return err
		}
	}
	logger.Info("Cluster deployment finalizer created nothing to do here ...: ")

	//checking i finalizers exits in the dmsi cr adding if they dont
	logger.Info("Checking for finalizers")
	if !utils.HasFinalizer(dmsi, deadMansSnitchFinalizer) {
		log.Info(fmt.Sprint("Adding finalizer to DMSI Name: ", " DMSI Name: :"+dmsi.Name))
		utils.AddFinalizer(dmsi, deadMansSnitchFinalizer)
		err := r.client.Update(context.TODO(), dmsi)
		if err != nil {
			return err
		}

	}
	logger.Info("DMSI finalizer created nothing to do here..: ")

	return nil

}

// create snitch in deadmanssnitch.com with information retrived from dmsi cr as well as the matching cluster deployment
func (r *ReconcileDeadmansSnitchIntegration) createSnitch(dmsi *deadmanssnitchv1alpha1.DeadmansSnitchIntegration, cd *hivev1.ClusterDeployment, dmsc dmsclient.Client) error {
	logger := log.WithValues("DeadMansSnitchIntegration.Namespace", dmsi.Namespace, "DMSI.Name", dmsi.Name, "cluster-deployment.Name:", cd.Name, "cluster-deployment.Namespace:", cd.Namespace)
	snitchName := utils.DmsSnitchName(cd.Spec.ClusterName, cd.Spec.BaseDomain, dmsi.Spec.SnitchNamePostFix)

	ssName := utils.SecretName(cd.Spec.ClusterName, dmsi.Spec.SnitchNamePostFix)
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: ssName, Namespace: cd.Namespace}, &hivev1.SyncSet{})

	if errors.IsNotFound(err) {
		logger.Info(fmt.Sprint("Checking if snitch already exists SnitchName:", snitchName))
		snitches, err := dmsc.FindSnitchesByName(snitchName)
		if err != nil {
			return err
		}

		var snitch dmsclient.Snitch
		if len(snitches) > 0 {
			snitch = snitches[0]
		} else {
			newSnitch := dmsclient.NewSnitch(snitchName, dmsi.Spec.Tags, "15_minute", "basic")
			newSnitch.Notes = fmt.Sprintf(`cluster_id: %s
runbook: https://github.com/openshift/ops-sop/blob/master/v4/alerts/cluster_has_gone_missing.md`, cd.Spec.ClusterMetadata.ClusterID)
			// add escaping since _ is not being recognized otherwise.
			newSnitch.Notes = "```" + newSnitch.Notes + "```"
			logger.Info(fmt.Sprint("Creating snitch:", snitchName))
			snitch, err = dmsc.Create(newSnitch)
			if err != nil {
				return err
			}
		}

		ReSnitches, err := dmsc.FindSnitchesByName(snitchName)
		if err != nil {
			return err
		}

		if len(ReSnitches) > 0 {
			if ReSnitches[0].Status == "pending" {
				logger.Info("Checking in Snitch ...")
				// CheckIn snitch
				err = dmsc.CheckIn(snitch)
				if err != nil {
					logger.Error(err, "Unable to check in deadman's snitch", "CheckInURL", snitch.CheckInURL)
					return err
				}
			}
		} else {
			logger.Error(err, "Unable to get Snitch by name")
			return err
		}
	}

	logger.Info("Snitch created nothing to do here.... ")
	return nil
}

// snitchResourcesExist checks if the associated cluster resources for a snitch exist
func (r *ReconcileDeadmansSnitchIntegration) snitchResourcesExist(dmsi *deadmanssnitchv1alpha1.DeadmansSnitchIntegration, cd *hivev1.ClusterDeployment) (bool, bool, error) {
	logger := log.WithValues("DeadMansSnitchIntegration.Namespace", dmsi.Namespace, "DMSI.Name", dmsi.Name, "cluster-deployment.Name:", cd.Name, "cluster-deployment.Namespace:", cd.Namespace)
	logger.Info("Checking for snitch resources")
	dmsSecret := utils.SecretName(cd.Spec.ClusterName, dmsi.Spec.SnitchNamePostFix)
	logger.Info("Checking if secret exists")
	secretExist := false
	err := r.client.Get(context.TODO(),
		types.NamespacedName{Name: dmsSecret, Namespace: cd.Namespace},
		&corev1.Secret{})
	if err != nil && !errors.IsNotFound(err) {
		return false, false, err
	}
	secretExist = !errors.IsNotFound(err)

	logger.Info("Checking if syncset exists")
	syncSetExist := false
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: dmsSecret, Namespace: cd.Namespace}, &hivev1.SyncSet{})
	if err != nil && !errors.IsNotFound(err) {
		return secretExist, false, err
	}
	syncSetExist = !errors.IsNotFound(err)

	return secretExist, syncSetExist, nil
}

//Create secret containing the snitch url
func (r *ReconcileDeadmansSnitchIntegration) createSecret(dmsi *deadmanssnitchv1alpha1.DeadmansSnitchIntegration, dmsc dmsclient.Client, cd hivev1.ClusterDeployment) error {
	logger := log.WithValues("DeadMansSnitchIntegration.Namespace", dmsi.Namespace, "DMSI.Name", dmsi.Name, "cluster-deployment.Name:", cd.Name, "cluster-deployment.Namespace:", cd.Namespace)
	dmsSecret := utils.SecretName(cd.Spec.ClusterName, dmsi.Spec.SnitchNamePostFix)
	logger.Info("Checking if secret already exits")
	err := r.client.Get(context.TODO(),
		types.NamespacedName{Name: dmsSecret, Namespace: cd.Namespace},
		&corev1.Secret{})

	if err != nil && !errors.IsNotFound(err) {
		logger.Error(err, "Failed to fetch secret")
		return err
	}

	if errors.IsNotFound(err) {
		logger.Info("Secret not found creating secret")
		snitchName := utils.DmsSnitchName(cd.Spec.ClusterName, cd.Spec.BaseDomain, dmsi.Spec.SnitchNamePostFix)
		ReSnitches, err := dmsc.FindSnitchesByName(snitchName)

		if err != nil {
			return err
		}
		for _, CheckInURL := range ReSnitches {

			newdmsSecret := newDMSSecret(cd.Namespace, dmsSecret, CheckInURL.CheckInURL)

			// set the owner reference about the secret for gabage collection
			if err := controllerutil.SetControllerReference(&cd, newdmsSecret, r.scheme); err != nil {
				logger.Error(err, "Error setting controller reference on secret")
				return err
			}
			// Create the secret
			if err := r.client.Create(context.TODO(), newdmsSecret); err != nil {
				logger.Error(err, "Failed to create secret")
				return err
			}

		}
	}
	logger.Info("Secret created, nothing to do here...")
	return nil
}

//creating the syncset which contain the secret with the snitch url
func (r *ReconcileDeadmansSnitchIntegration) createSyncset(dmsi *deadmanssnitchv1alpha1.DeadmansSnitchIntegration, cd hivev1.ClusterDeployment) error {
	logger := log.WithValues("DeadMansSnitchIntegration.Namespace", dmsi.Namespace, "DMSI.Name", dmsi.Name, "cluster-deployment.Name:", cd.Name, "cluster-deployment.Namespace:", cd.Namespace)
	ssName := utils.SecretName(cd.Spec.ClusterName, dmsi.Spec.SnitchNamePostFix)
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: ssName, Namespace: cd.Namespace}, &hivev1.SyncSet{})

	if errors.IsNotFound(err) {
		logger.Info("SyncSet not found, Creating a new SyncSet")

		newSS := newSyncSet(cd.Namespace, ssName, cd.Name, dmsi)
		if err := controllerutil.SetControllerReference(&cd, newSS, r.scheme); err != nil {
			logger.Error(err, "Error setting controller reference on syncset")
			return err
		}
		if err := r.client.Create(context.TODO(), newSS); err != nil {
			logger.Error(err, "Error creating syncset")
			return err
		}
		logger.Info("Done creating a new SyncSet")

	} else {
		logger.Info("SyncSet Already Present, nothing to do here...")
		// return directly if the syscset already existed

	}
	return nil

}

func newDMSSecret(namespace string, name string, snitchURL string) *corev1.Secret {

	dmsSecret := &corev1.Secret{
		Type: "Opaque",
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			config.KeySnitchURL: []byte(snitchURL),
		},
	}

	return dmsSecret

}

func newSyncSet(namespace string, dmsSecret string, clusterDeploymentName string, dmsi *deadmanssnitchv1alpha1.DeadmansSnitchIntegration) *hivev1.SyncSet {

	newSS := &hivev1.SyncSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dmsSecret,
			Namespace: namespace,
		},
		Spec: hivev1.SyncSetSpec{
			ClusterDeploymentRefs: []corev1.LocalObjectReference{
				{
					Name: clusterDeploymentName,
				},
			},
			SyncSetCommonSpec: hivev1.SyncSetCommonSpec{
				ResourceApplyMode: hivev1.SyncResourceApplyMode,
				Secrets: []hivev1.SecretMapping{
					{
						SourceRef: hivev1.SecretReference{
							Name:      dmsSecret,
							Namespace: namespace,
						},
						TargetRef: hivev1.SecretReference{
							Name:      dmsi.Spec.TargetSecretRef.Name,
							Namespace: dmsi.Spec.TargetSecretRef.Namespace,
						},
					},
				},
			},
		},
	}

	return newSS

}

// delete snitches,secrets and syncset associated with the cluster deployment that has been deleted
func (r *ReconcileDeadmansSnitchIntegration) deleteDMSClusterDeployment(dmsi *deadmanssnitchv1alpha1.DeadmansSnitchIntegration, clusterDeployment *hivev1.ClusterDeployment, dmsc dmsclient.Client) error {
	deadMansSnitchFinalizer := DeadMansSnitchFinalizerPrefix + dmsi.Name
	logger := log.WithValues("DeadMansSnitchIntegration.Namespace", dmsi.Namespace, "DMSI.Name", dmsi.Name, "cluster-deployment.Name:", clusterDeployment.Name, "cluster-deployment.Namespace:", clusterDeployment.Namespace)

	// Delete the dms
	logger.Info("Deleting the DMS from api.deadmanssnitch.com")
	snitchName := utils.DmsSnitchName(clusterDeployment.Spec.ClusterName, clusterDeployment.Spec.BaseDomain, dmsi.Spec.SnitchNamePostFix)
	snitches, err := dmsc.FindSnitchesByName(snitchName)
	if err != nil {
		return err
	}
	for _, s := range snitches {
		delStatus, err := dmsc.Delete(s.Token)
		if !delStatus || err != nil {
			logger.Error(err, "Failed to delete the DMS from api.deadmanssnitch.com")
			return err
		}
		logger.Info("Deleted the DMS from api.deadmanssnitch.com")
	}

	// Delete the SyncSet
	logger.Info("Deleting DMS SyncSet")
	dmsSecret := utils.SecretName(clusterDeployment.Spec.ClusterName, dmsi.Spec.SnitchNamePostFix)
	err = utils.DeleteSyncSet(dmsSecret, clusterDeployment.Namespace, r.client)
	if err != nil {
		logger.Error(err, "Error deleting SyncSet")
		return err
	}

	// Delete the referenced secret
	logger.Info("Deleting DMS referenced secret")
	err = utils.DeleteRefSecret(dmsSecret, clusterDeployment.Namespace, r.client)
	if err != nil {
		logger.Error(err, "Error deleting secret")
		return err
	}

	if utils.HasFinalizer(clusterDeployment, deadMansSnitchFinalizer) {
		logger.Info("Deleting DMSI finalizer from cluster deployment")
		baseToPatch := client.MergeFrom(clusterDeployment.DeepCopy())
		utils.DeleteFinalizer(clusterDeployment, deadMansSnitchFinalizer)
		if err := r.client.Patch(context.TODO(), clusterDeployment, baseToPatch); err != nil {
			logger.Error(err, "Error deleting Finalizer from cluster deployment")
			return err
		}
	}

	return nil

}

func instancesAreRunning(cd hivev1.ClusterDeployment) bool {
	// Get hibernation PowerState a new ClusterDeployment Status field indicating if the cluster is running
	// ie. The cluster is not "Resuming" if the PowerState is "Running", the cluster is operational.
	// If the field is blank we move on and check the legacy reasons (It may be blank if the running version of
	// Hive on cluster doesn't yet support it)
	if cd.Status.PowerState == "Running" {
		return true
	}

	// This can be removed once Hive is promoted past f73ed3e in all environments
	// We can rely on ClusterDeployment.Status.PowerState
	hibernatingCondition := getCondition(cd.Status.Conditions, hivev1.ClusterHibernatingCondition)

	// Verify the ClusterDeployment has a hibernation condition
	if hibernatingCondition == nil {
		return false
	}

	// Verify the hibernatingCondition is not active (ConditionTrue and ConditionUnknown are discarded)
	if hibernatingCondition.Status != corev1.ConditionFalse {
		return false
	}

	// Check legacy Hibernation condition reasons
	return hibernatingCondition.Reason == legacyHivev1RunningHibernationReason
}

func getCondition(conditions []hivev1.ClusterDeploymentCondition, t hivev1.ClusterDeploymentConditionType) *hivev1.ClusterDeploymentCondition {
	for _, condition := range conditions {
		if condition.Type == t {
			return &condition
		}
	}
	return nil
}
