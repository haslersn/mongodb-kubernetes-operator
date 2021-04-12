package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/mongodb/mongodb-kubernetes-operator/controllers/predicates"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"time"

	"github.com/mongodb/mongodb-kubernetes-operator/pkg/agent"

	"github.com/mongodb/mongodb-kubernetes-operator/pkg/util/result"

	"github.com/mongodb/mongodb-kubernetes-operator/pkg/util/scale"

	"github.com/pkg/errors"

	"github.com/imdario/mergo"
	"github.com/mongodb/mongodb-kubernetes-operator/pkg/authentication/scram"
	"github.com/stretchr/objx"

	"github.com/mongodb/mongodb-kubernetes-operator/controllers/construct"
	"github.com/mongodb/mongodb-kubernetes-operator/controllers/validation"
	"github.com/mongodb/mongodb-kubernetes-operator/controllers/watch"

	"github.com/mongodb/mongodb-kubernetes-operator/pkg/kube/annotations"
	"github.com/mongodb/mongodb-kubernetes-operator/pkg/kube/podtemplatespec"

	mdbv1 "github.com/mongodb/mongodb-kubernetes-operator/api/v1"
	"github.com/mongodb/mongodb-kubernetes-operator/pkg/automationconfig"
	kubernetesClient "github.com/mongodb/mongodb-kubernetes-operator/pkg/kube/client"
	"github.com/mongodb/mongodb-kubernetes-operator/pkg/kube/service"
	"github.com/mongodb/mongodb-kubernetes-operator/pkg/kube/statefulset"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sClient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	clusterDNSName = "CLUSTER_DNS_NAME"

	lastSuccessfulConfiguration = "mongodb.com/v1.lastSuccessfulConfiguration"
)

func init() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		os.Exit(1)
	}
	zap.ReplaceGlobals(logger)
}

func NewReconciler(mgr manager.Manager) *ReplicaSetReconciler {
	mgrClient := mgr.GetClient()
	secretWatcher := watch.New()

	return &ReplicaSetReconciler{
		client:        kubernetesClient.NewClient(mgrClient),
		scheme:        mgr.GetScheme(),
		log:           zap.S(),
		secretWatcher: &secretWatcher,
	}
}

// SetupWithManager sets up the controller with the Manager and configures the necessary watches.
func (r *ReplicaSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mdbv1.MongoDBCommunity{}, builder.WithPredicates(predicates.OnlyOnSpecChange())).
		Complete(r)
}

// ReplicaSetReconciler reconciles a MongoDB ReplicaSet
type ReplicaSetReconciler struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client        kubernetesClient.Client
	scheme        *runtime.Scheme
	log           *zap.SugaredLogger
	secretWatcher *watch.ResourceWatcher
}

// +kubebuilder:rbac:groups=mongodbcommunity.mongodb.com,resources=mongodbcommunity,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mongodbcommunity.mongodb.com,resources=mongodbcommunity/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mongodbcommunity.mongodb.com,resources=mongodbcommunity/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list

// Reconcile reads that state of the cluster for a MongoDB object and makes changes based on the state read
// and what is in the MongoDB.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r ReplicaSetReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {

	time.Sleep(5 * time.Second)
	// TODO: generalize preparation for resource
	// Fetch the MongoDB instance
	mdb := mdbv1.MongoDBCommunity{}
	err := r.client.Get(context.TODO(), request.NamespacedName, &mdb)
	if err != nil {
		if apiErrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return result.OK()
		}
		r.log.Errorf("Error reconciling MongoDB resource: %s", err)
		// Error reading the object - requeue the request.
		return result.Failed()
	}

	log := zap.S().With("ReplicaSet", mdb.Namespace)

	// Determine current state
	sm := BuildStateMachine(r.client, mdb, r.secretWatcher, log)

	startingStateName, err := getLastStateName(mdb)
	if err != nil {
		log.Errorf("Error fetching last state name from MongoDBCommunity annotations: %s", err)
		return reconcile.Result{}, err
	}
	startingState, ok := sm.States[startingStateName]
	if !ok {
		log.Errorf("Attempted to set starting state to %s, but it was not registered with the State Machine!", startingStateName)
		return reconcile.Result{}, nil
	}

	a, err := getAllStates(mdb)
	if err != nil {
		log.Errorf("Error getting all States: %s", err)
		return reconcile.Result{}, nil
	}

	log.Infof("Current state completion %s: %s", a.CurrentState, a.StateCompletionStatus[a.CurrentState])

	sm.SetState(startingState, true)
	return sm.Reconcile()

	//// the last version will be duplicated in two annotations.
	//// This is needed to reuse the update strategy logic in enterprise
	//if err := annotations.UpdateLastAppliedMongoDBVersion(&mdb, r.client); err != nil {
	//	r.log.Errorf("Could not save current version as an annotation: %s", err)
	//}
	//if err := r.updateLastSuccessfulConfiguration(mdb); err != nil {
	//	r.log.Errorf("Could not save current spec as an annotation: %s", err)
	//}
	//
	//if res.RequeueAfter > 0 || res.Requeue {
	//	r.log.Infow("Requeuing reconciliation", "MongoDB.Spec:", mdb.Spec, "MongoDB.Status:", mdb.Status)
	//	return res, nil
	//}
	//
	//r.log.Infow("Successfully finished reconciliation", "MongoDB.Spec:", mdb.Spec, "MongoDB.Status:", mdb.Status)
	//return res, err
}

// updateLastSuccessfulConfiguration annotates the MongoDBCommunity resource with the latest configuration
func (r *ReplicaSetReconciler) updateLastSuccessfulConfiguration(mdb mdbv1.MongoDBCommunity) error {
	currentSpec, err := json.Marshal(mdb.Spec)
	if err != nil {
		return err
	}

	specAnnotations := map[string]string{
		lastSuccessfulConfiguration: string(currentSpec),
	}
	return annotations.SetAnnotations(&mdb, specAnnotations, r.client)
}

// ensureTLSResources creates any required TLS resources that the MongoDBCommunity
// requires for TLS configuration.
func (r *ReplicaSetReconciler) ensureTLSResources(mdb mdbv1.MongoDBCommunity) error {
	if !mdb.Spec.Security.TLS.Enabled {
		return nil
	}
	// the TLS secret needs to be created beforehand, as both the StatefulSet and AutomationConfig
	// require the contents.
	if mdb.Spec.Security.TLS.Enabled {
		r.log.Infof("TLS is enabled, creating/updating TLS secret")
		if err := ensureTLSSecret(r.client, mdb); err != nil {
			return errors.Errorf("could not ensure TLS secret: %s", err)
		}
	}
	return nil
}

// deployStatefulSet deploys the backing StatefulSet of the MongoDBCommunity resource.
// The returned boolean indicates that the StatefulSet is ready.
func deployStatefulSet(client kubernetesClient.Client, mdb mdbv1.MongoDBCommunity, log *zap.SugaredLogger) (bool, error) {
	log.Info("Creating/Updating StatefulSet")
	if err := createOrUpdateStatefulSet(client, mdb); err != nil {
		return false, errors.Errorf("error creating/updating StatefulSet: %s", err)
	}

	currentSts, err := client.GetStatefulSet(mdb.NamespacedName())
	if err != nil {
		return false, errors.Errorf("error getting StatefulSet: %s", err)
	}

	log.Debugf("Ensuring StatefulSet is ready, with type: %s", mdb.GetUpdateStrategyType())

	isReady := statefulset.IsReady(currentSts, mdb.StatefulSetReplicasThisReconciliation())

	if isReady {
		log.Infow("StatefulSet is ready",
			"replicas", currentSts.Spec.Replicas,
			"generation", currentSts.Generation,
			"observedGeneration", currentSts.Status.ObservedGeneration,
			"updateStrategy", currentSts.Spec.UpdateStrategy.Type,
		)
	}

	return isReady || currentSts.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType, nil
}

// deployAutomationConfig deploys the AutomationConfig for the MongoDBCommunity resource.
// The returned boolean indicates whether or not that Agents have all reached goal state.
func deployAutomationConfig(client kubernetesClient.Client, mdb mdbv1.MongoDBCommunity, log *zap.SugaredLogger) (bool, error) {
	log.Infof("Creating/Updating AutomationConfig")

	sts, err := client.GetStatefulSet(mdb.NamespacedName())
	if err != nil && !apiErrors.IsNotFound(err) {
		return false, fmt.Errorf("failed to get StatefulSet: %s", err)
	}

	ac, err := ensureAutomationConfig(client, mdb)
	if err != nil {
		return false, fmt.Errorf("failed to ensure AutomationConfig: %s", err)
	}

	// the StatefulSet has not yet been created, so the next stage of reconciliation will be
	// creating the StatefulSet and ensuring it reaches the Running phase.
	if apiErrors.IsNotFound(err) {
		return true, nil
	}

	log.Debugf("Waiting for agents to reach version %d", ac.Version)
	// Note: we pass in the expected number of replicas this reconciliation as we scale members one at a time. If we were
	// to pass in the final member count, we would be waiting for agents that do not exist yet to be ready.
	ready, err := agent.AllReachedGoalState(sts, client, mdb.StatefulSetReplicasThisReconciliation(), ac.Version, log)
	if err != nil {
		return false, fmt.Errorf("failed to ensure agents have reached goal state: %s", err)
	}

	return ready, nil
}

// needToPublishStateFirst returns true if the order of execution of the AutomationConfig & StatefulSet
// functions should be sequential or not. A value of false indicates they will run in reversed order.
func needToPublishStateFirst(client kubernetesClient.Client, mdb mdbv1.MongoDBCommunity, log *zap.SugaredLogger) bool {
	// The only case when we push the StatefulSet first is when we are ensuring TLS for the already existing ReplicaSet
	_, err := client.GetStatefulSet(mdb.NamespacedName())
	if err == nil && mdb.Spec.Security.TLS.Enabled {
		log.Debug("Enabling TLS on an existing deployment, the StatefulSet must be updated first")
		return false
	}

	// if we are scaling up, we need to make sure the StatefulSet is scaled up first.
	if scale.IsScalingUp(mdb) {
		log.Debug("Scaling up the ReplicaSet, the StatefulSet must be updated first")
		return false
	}

	if scale.IsScalingDown(mdb) {
		log.Debug("Scaling down the ReplicaSet, the Automation Config must be updated first")
		return true
	}

	// when we change version, we need the StatefulSet images to be updated first, then the agent can get to goal
	// state on the new version.
	if mdb.IsChangingVersion() {
		log.Debug("Version change in progress, the StatefulSet must be updated first")
		return false
	}

	return true
}

func createOrUpdateStatefulSet(client kubernetesClient.Client, mdb mdbv1.MongoDBCommunity) error {
	set := appsv1.StatefulSet{}
	err := client.Get(context.TODO(), mdb.NamespacedName(), &set)
	err = k8sClient.IgnoreNotFound(err)
	if err != nil {
		return errors.Errorf("error getting StatefulSet: %s", err)
	}
	buildStatefulSetModificationFunction(mdb)(&set)
	if _, err = statefulset.CreateOrUpdate(client, set); err != nil {
		return errors.Errorf("error creating/updating StatefulSet: %s", err)
	}
	return nil
}

// ensureAutomationConfig makes sure the AutomationConfig secret has been successfully created. The automation config
// that was updated/created is returned.
func ensureAutomationConfig(client kubernetesClient.Client, mdb mdbv1.MongoDBCommunity) (automationconfig.AutomationConfig, error) {
	ac, err := buildAutomationConfigController(client, mdb)
	if err != nil {
		return automationconfig.AutomationConfig{}, errors.Errorf("could not build automation config: %s", err)
	}

	return automationconfig.EnsureSecret(
		client,
		types.NamespacedName{Name: mdb.AutomationConfigSecretName(), Namespace: mdb.Namespace},
		[]metav1.OwnerReference{getOwnerReference(mdb)},
		ac,
	)

}

func buildAutomationConfig(mdb mdbv1.MongoDBCommunity, auth automationconfig.Auth, currentAc automationconfig.AutomationConfig, modifications ...automationconfig.Modification) (automationconfig.AutomationConfig, error) {
	domain := getDomain(mdb.ServiceName(), mdb.Namespace, os.Getenv(clusterDNSName))
	zap.S().Debugw("AutomationConfigMembersThisReconciliation", "mdb.AutomationConfigMembersThisReconciliation()", mdb.AutomationConfigMembersThisReconciliation())

	return automationconfig.NewBuilder().
		SetTopology(automationconfig.ReplicaSetTopology).
		SetName(mdb.Name).
		SetDomain(domain).
		SetMembers(mdb.AutomationConfigMembersThisReconciliation()).
		SetReplicaSetHorizons(mdb.Spec.ReplicaSetHorizons).
		SetPreviousAutomationConfig(currentAc).
		SetMongoDBVersion(mdb.Spec.Version).
		SetFCV(mdb.Spec.FeatureCompatibilityVersion).
		SetOptions(automationconfig.Options{DownloadBase: "/var/lib/mongodb-mms-automation"}).
		SetAuth(auth).
		AddModifications(getMongodConfigModification(mdb)).
		AddModifications(modifications...).
		Build()
}

// buildService creates a Service that will be used for the Replica Set StatefulSet
// that allows all the members of the STS to see each other.
// TODO: Make sure this Service is as minimal as possible, to not interfere with
// future implementations and Service Discovery mechanisms we might implement.
func buildService(mdb mdbv1.MongoDBCommunity) corev1.Service {
	label := make(map[string]string)
	label["app"] = mdb.ServiceName()
	return service.Builder().
		SetName(mdb.ServiceName()).
		SetNamespace(mdb.Namespace).
		SetSelector(label).
		SetServiceType(corev1.ServiceTypeClusterIP).
		SetClusterIP("None").
		SetPort(27017).
		SetPublishNotReadyAddresses(true).
		SetOwnerReferences([]metav1.OwnerReference{getOwnerReference(mdb)}).
		Build()
}

// validateUpdate validates that the new Spec, corresponding to the existing one
// is still valid. If there is no a previous Spec, then the function assumes this is
// the first version of the MongoDB resource and skips.
func validateUpdate(mdb mdbv1.MongoDBCommunity) error {
	lastSuccessfulConfigurationSaved, ok := mdb.Annotations[lastSuccessfulConfiguration]
	if !ok {
		// First version of Spec, no need to validate
		return nil
	}

	prevSpec := mdbv1.MongoDBCommunitySpec{}
	err := json.Unmarshal([]byte(lastSuccessfulConfigurationSaved), &prevSpec)
	if err != nil {
		return err
	}

	return validation.Validate(prevSpec, mdb.Spec)
}

func getCustomRolesModification(mdb mdbv1.MongoDBCommunity) (automationconfig.Modification, error) {
	roles := mdb.Spec.Security.Roles
	if roles == nil {
		return automationconfig.NOOP(), nil
	}

	return func(config *automationconfig.AutomationConfig) {
		config.Roles = mdbv1.ConvertCustomRolesToAutomationConfigCustomRole(roles)
	}, nil
}

func buildAutomationConfigController(client kubernetesClient.Client, mdb mdbv1.MongoDBCommunity) (automationconfig.AutomationConfig, error) {
	tlsModification, err := getTLSConfigModification(client, mdb)
	if err != nil {
		return automationconfig.AutomationConfig{}, errors.Errorf("could not configure TLS modification: %s", err)
	}

	customRolesModification, err := getCustomRolesModification(mdb)
	if err != nil {
		return automationconfig.AutomationConfig{}, errors.Errorf("could not configure custom roles: %s", err)
	}

	currentAC, err := automationconfig.ReadFromSecret(client, types.NamespacedName{Name: mdb.AutomationConfigSecretName(), Namespace: mdb.Namespace})
	if err != nil {
		return automationconfig.AutomationConfig{}, errors.Errorf("could not read existing automation config: %s", err)
	}

	auth := automationconfig.Auth{}
	if err := scram.Enable(&auth, client, mdb); err != nil {
		return automationconfig.AutomationConfig{}, errors.Errorf("could not configure scram authentication: %s", err)
	}

	return buildAutomationConfig(
		mdb,
		auth,
		currentAC,
		tlsModification,
		customRolesModification,
	)
}

// getMongodConfigModification will merge the additional configuration in the CRD
// into the configuration set up by the operator.
func getMongodConfigModification(mdb mdbv1.MongoDBCommunity) automationconfig.Modification {
	return func(ac *automationconfig.AutomationConfig) {
		for i := range ac.Processes {
			// Mergo requires both objects to have the same type
			// TODO: handle this error gracefully, we may need to add an error as second argument for all modification functions
			_ = mergo.Merge(&ac.Processes[i].Args26, objx.New(mdb.Spec.AdditionalMongodConfig.Object), mergo.WithOverride)
		}
	}
}

// buildStatefulSet takes a MongoDB resource and converts it into
// the corresponding stateful set
func buildStatefulSet(mdb mdbv1.MongoDBCommunity) (appsv1.StatefulSet, error) {
	sts := appsv1.StatefulSet{}
	buildStatefulSetModificationFunction(mdb)(&sts)
	return sts, nil
}

func buildStatefulSetModificationFunction(mdb mdbv1.MongoDBCommunity) statefulset.Modification {
	commonModification := construct.BuildMongoDBReplicaSetStatefulSetModificationFunction(&mdb, mdb)
	return statefulset.Apply(
		commonModification,
		statefulset.WithOwnerReference([]metav1.OwnerReference{getOwnerReference(mdb)}),
		statefulset.WithPodSpecTemplate(
			podtemplatespec.Apply(
				buildTLSPodSpecModification(mdb),
			),
		),

		statefulset.WithCustomSpecs(mdb.Spec.StatefulSetConfiguration.SpecWrapper.Spec),
	)
}

func getOwnerReference(mdb mdbv1.MongoDBCommunity) metav1.OwnerReference {
	return *metav1.NewControllerRef(&mdb, schema.GroupVersionKind{
		Group:   mdbv1.GroupVersion.Group,
		Version: mdbv1.GroupVersion.Version,
		Kind:    mdb.Kind,
	})
}

func getDomain(service, namespace, clusterName string) string {
	if clusterName == "" {
		clusterName = "cluster.local"
	}
	return fmt.Sprintf("%s.%s.svc.%s", service, namespace, clusterName)
}
