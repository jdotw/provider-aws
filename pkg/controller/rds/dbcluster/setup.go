package dbcluster

import (
	"context"
	"time"

	svcsdk "github.com/aws/aws-sdk-go/service/rds"
	svcsdkapi "github.com/aws/aws-sdk-go/service/rds/rdsiface"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/password"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	svcapitypes "github.com/crossplane/provider-aws/apis/rds/v1alpha1"
	aws "github.com/crossplane/provider-aws/pkg/clients"
	"github.com/crossplane/provider-aws/pkg/clients/rds"
)

const (
	errGetSecretFailed    = "failed to get Kubernetes secret"
	errUpdateSecretFailed = "failed to update Kubernetes secret"
	errSaveSecretFailed   = "failed to save generated password to Kubernetes secret"
)

// SetupDBCluster adds a controller that reconciles DbCluster.
func SetupDBCluster(mgr ctrl.Manager, l logging.Logger, rl workqueue.RateLimiter, poll time.Duration) error {
	name := managed.ControllerName(svcapitypes.DBClusterGroupKind)
	opts := []option{
		func(e *external) {
			e.preObserve = preObserve
			e.postObserve = postObserve
			c := &custom{client: e.client, kube: e.kube}
			e.isUpToDate = isUpToDate
			e.preUpdate = preUpdate
			e.preCreate = c.preCreate
			e.postCreate = c.postCreate
			e.preDelete = preDelete
			e.filterList = filterList
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(controller.Options{
			RateLimiter: ratelimiter.NewController(rl),
		}).
		For(&svcapitypes.DBCluster{}).
		Complete(managed.NewReconciler(mgr,
			resource.ManagedKind(svcapitypes.DBClusterGroupVersionKind),
			managed.WithExternalConnecter(&connector{kube: mgr.GetClient(), opts: opts}),
			managed.WithPollInterval(poll),
			managed.WithLogger(l.WithValues("controller", name)),
			managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name)))))
}

func preObserve(_ context.Context, cr *svcapitypes.DBCluster, obj *svcsdk.DescribeDBClustersInput) error {
	obj.DBClusterIdentifier = aws.String(meta.GetExternalName(cr))
	return nil
}

// This probably requires custom Conditions to be defined for handling all statuses
// described here https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/Aurora.Status.html
// Need to get help from community on how to deal with this. Ideally the status should reflect
// the true status value as described by the provider.
func postObserve(_ context.Context, cr *svcapitypes.DBCluster, resp *svcsdk.DescribeDBClustersOutput, obs managed.ExternalObservation, err error) (managed.ExternalObservation, error) {
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	switch aws.StringValue(resp.DBClusters[0].Status) {
	case "available", "modifying":
		cr.SetConditions(xpv1.Available())
	case "deleting", "stopped", "stopping":
		cr.SetConditions(xpv1.Unavailable())
	case "creating":
		cr.SetConditions(xpv1.Creating())
	}
	return obs, nil
}

type custom struct {
	kube   client.Client
	client svcsdkapi.RDSAPI
}

func (e *custom) preCreate(ctx context.Context, cr *svcapitypes.DBCluster, obj *svcsdk.CreateDBClusterInput) error {
	pw, _, err := rds.GetPassword(ctx, e.kube, cr.Spec.ForProvider.MasterUserPasswordSecretRef, cr.Spec.WriteConnectionSecretToReference)
	if resource.IgnoreNotFound(err) != nil {
		return errors.Wrap(err, "cannot get password from the given secret")
	}
	if pw == "" && cr.Spec.ForProvider.AutogeneratePassword != nil && *cr.Spec.ForProvider.AutogeneratePassword {
		pw, err = password.Generate()
		if err != nil {
			return errors.Wrap(err, "unable to generate a password")
		}
		if err := e.savePasswordSecret(ctx, cr, pw); err != nil {
			return errors.Wrap(err, errSaveSecretFailed)
		}
	}
	obj.MasterUserPassword = aws.String(pw)
	obj.DBClusterIdentifier = aws.String(meta.GetExternalName(cr))
	obj.VpcSecurityGroupIds = make([]*string, len(cr.Spec.ForProvider.VPCSecurityGroupIDs))
	for i, v := range cr.Spec.ForProvider.VPCSecurityGroupIDs {
		obj.VpcSecurityGroupIds[i] = aws.String(v)
	}
	return nil
}

func (e *custom) postCreate(ctx context.Context, cr *svcapitypes.DBCluster, out *svcsdk.CreateDBClusterOutput, ec managed.ExternalCreation, err error) (managed.ExternalCreation, error) {
	if err != nil {
		return managed.ExternalCreation{}, err
	}
	conn := managed.ConnectionDetails{
		xpv1.ResourceCredentialsSecretEndpointKey: []byte(aws.StringValue(cr.Status.AtProvider.Endpoint)),
		xpv1.ResourceCredentialsSecretUserKey:     []byte(aws.StringValue(cr.Spec.ForProvider.MasterUsername)),
	}
	pw, _, err := rds.GetPassword(ctx, e.kube, cr.Spec.ForProvider.MasterUserPasswordSecretRef, cr.Spec.WriteConnectionSecretToReference)
	if resource.IgnoreNotFound(err) != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, "cannot get password from the given secret")
	}
	if pw != "" {
		conn[xpv1.ResourceCredentialsSecretPasswordKey] = []byte(pw)
	} else {
		conn[xpv1.ResourceCredentialsSecretPasswordKey] = []byte(*out.DBCluster.PendingModifiedValues.MasterUserPassword)
	}
	return managed.ExternalCreation{
		ConnectionDetails: conn,
	}, nil
}

func isUpToDate(cr *svcapitypes.DBCluster, out *svcsdk.DescribeDBClustersOutput) (bool, error) {
	status := aws.StringValue(out.DBClusters[0].Status)
	if status == "modifying" || status == "upgrading" || status == "configuring-iam-database-auth" {
		return true, nil
	}

	if aws.BoolValue(cr.Spec.ForProvider.EnableIAMDatabaseAuthentication) != aws.BoolValue(out.DBClusters[0].IAMDatabaseAuthenticationEnabled) {
		return false, nil
	}

	return true, nil
}

func preUpdate(_ context.Context, cr *svcapitypes.DBCluster, obj *svcsdk.ModifyDBClusterInput) error {
	obj.DBClusterIdentifier = aws.String(meta.GetExternalName(cr))
	obj.ApplyImmediately = cr.Spec.ForProvider.ApplyImmediately

	return nil
}

func preDelete(_ context.Context, cr *svcapitypes.DBCluster, obj *svcsdk.DeleteDBClusterInput) (bool, error) {
	obj.DBClusterIdentifier = aws.String(meta.GetExternalName(cr))
	obj.FinalDBSnapshotIdentifier = aws.String(cr.Spec.ForProvider.FinalDBSnapshotIdentifier)
	obj.SkipFinalSnapshot = aws.Bool(cr.Spec.ForProvider.SkipFinalSnapshot)
	return false, nil
}

func filterList(cr *svcapitypes.DBCluster, obj *svcsdk.DescribeDBClustersOutput) *svcsdk.DescribeDBClustersOutput {
	clusterIdentifier := aws.String(meta.GetExternalName(cr))
	resp := &svcsdk.DescribeDBClustersOutput{}
	for _, dbCluster := range obj.DBClusters {
		if aws.StringValue(dbCluster.DBClusterIdentifier) == aws.StringValue(clusterIdentifier) {
			resp.DBClusters = append(resp.DBClusters, dbCluster)
			break
		}
	}
	return resp
}

func (e *custom) savePasswordSecret(ctx context.Context, cr *svcapitypes.DBCluster, pw string) error {
	if cr.Spec.ForProvider.MasterUserPasswordSecretRef == nil {
		return errors.New("no MasterUserPasswordSecretRef given, unable to save password")
	}
	ref := cr.Spec.ForProvider.MasterUserPasswordSecretRef
	nn := types.NamespacedName{
		Name:      ref.Name,
		Namespace: ref.Namespace,
	}
	sc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nn.Name,
			Namespace: nn.Namespace,
		},
	}
	err := e.kube.Get(ctx, nn, sc)
	var create bool
	// if there was an error not related to the output secret not existing we should exit
	if resource.IgnoreNotFound(err) != nil {
		return errors.Wrap(err, errGetSecretFailed)
	}
	// but if it didn't exist, we should create instead of update
	if err != nil {
		create = true
	}
	sc.StringData = map[string]string{
		ref.Key: pw,
	}
	if create {
		err = e.kube.Create(ctx, sc, &client.CreateOptions{})
	} else {
		err = e.kube.Update(ctx, sc, &client.UpdateOptions{})
	}
	if err != nil {
		return errors.Wrap(err, errUpdateSecretFailed)
	}
	return nil
}
