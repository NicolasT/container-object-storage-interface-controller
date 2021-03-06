package bucketaccessrequest

import (
	"context"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubernetes-sigs/container-object-storage-interface-api/apis/objectstorage.k8s.io/v1alpha1"
	bucketclientset "github.com/kubernetes-sigs/container-object-storage-interface-api/clientset"
	bucketcontroller "github.com/kubernetes-sigs/container-object-storage-interface-api/controller"
	"github.com/kubernetes-sigs/container-object-storage-interface-controller/pkg/util"
	kubeclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/golang/glog"
)

type bucketAccessRequestListener struct {
	kubeClient   kubeclientset.Interface
	bucketClient bucketclientset.Interface
}

func NewListener() bucketcontroller.BucketAccessRequestListener {
	return &bucketAccessRequestListener{}
}

func (b *bucketAccessRequestListener) InitializeKubeClient(k kubeclientset.Interface) {
	b.kubeClient = k
}

func (b *bucketAccessRequestListener) InitializeBucketClient(bc bucketclientset.Interface) {
	b.bucketClient = bc
}

func (b *bucketAccessRequestListener) Add(ctx context.Context, obj *v1alpha1.BucketAccessRequest) error {
	glog.V(1).Infof("Add called for BucketAccessRequest %s", obj.Name)
	bucketAccessRequest := obj

	err := b.provisionBucketAccess(ctx, bucketAccessRequest)
	if err != nil {
		// Provisioning is 100% finished / not in progress.
		switch err {
		case util.ErrInvalidBucketAccessClass:
			glog.V(1).Infof("BucketAccessClass specified does not exist while processing BucketAccessRequest %v.", bucketAccessRequest.Name)
			err = nil
		case util.ErrBucketAccessAlreadyExists:
			glog.V(1).Infof("BucketAccess already exist for this BucketAccessRequest %v.", bucketAccessRequest.Name)
			err = nil
		default:
			glog.V(1).Infof("Error occurred processing BucketAccessRequest %v: %v", bucketAccessRequest.Name, err)
		}
		return err
	}

	glog.V(1).Infof("BucketAccessRequest %v is successfully processed.", bucketAccessRequest.Name)
	return nil
}

func (b *bucketAccessRequestListener) Update(ctx context.Context, old, new *v1alpha1.BucketAccessRequest) error {
	glog.V(1).Infof("Update called for BucketAccessRequest %v", old.Name)
	return nil
}

func (b *bucketAccessRequestListener) Delete(ctx context.Context, obj *v1alpha1.BucketAccessRequest) error {
	glog.V(1).Infof("Delete called for BucketAccessRequest %v", obj.Name)
	return nil
}

// provisionBucketAccess  attempts to provision a BucketAccess for the given bucketAccessRequest.
// Returns nil error only when the bucketaccess was provisioned. An error is return if we cannot create bucket access.
// A normal error is returned when  bucket acess  was not provisioned and provisioning should be retried (requeue the bucketAccessRequest),
// or a special error  errBucketAccessAlreadyExists, errInvalidBucketAccessClass is returned when provisioning was impossible and
// no further attempts to provision should be tried.
func (b *bucketAccessRequestListener) provisionBucketAccess(ctx context.Context, bucketAccessRequest *v1alpha1.BucketAccessRequest) error {
	bucketAccessClassName := bucketAccessRequest.Spec.BucketAccessClassName

	bucketaccess := b.FindBucketAccess(ctx, bucketAccessRequest)
	if bucketaccess != nil {
		// bucketaccess has provisioned, nothing to do.
		return util.ErrBucketAccessAlreadyExists
	}

	bucketAccessClass, err := b.bucketClient.ObjectstorageV1alpha1().BucketAccessClasses().Get(ctx, bucketAccessClassName, metav1.GetOptions{})
	if bucketAccessClass == nil {
		// bucket access class is invalid or not specified, cannot continue with provisioning.
		return util.ErrInvalidBucketAccessClass
	}

	bucketRequest, err := b.bucketClient.ObjectstorageV1alpha1().BucketRequests(bucketAccessRequest.Namespace).Get(ctx, bucketAccessRequest.Spec.BucketRequestName, metav1.GetOptions{})
	if bucketRequest == nil {
		// bucket request does not exist, we have to reject this provision.
		return util.ErrInvalidBucketRequest
	}
	if err != nil {
		return err
	}

	if bucketRequest.Spec.BucketInstanceName == "" {
		return util.ErrWaitForBucketProvisioning
	}

	sa, err := b.kubeClient.CoreV1().ServiceAccounts(bucketAccessRequest.Namespace).Get(ctx, bucketAccessRequest.Spec.ServiceAccountName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	bucketaccess = &v1alpha1.BucketAccess{}
	bucketaccess.Name = util.GetUUID()

	bucketaccess.Spec.BucketInstanceName = bucketRequest.Spec.BucketInstanceName
	bucketaccess.Spec.BucketAccessRequest = &v1.ObjectReference{
		Name:      bucketAccessRequest.Name,
		Namespace: bucketAccessRequest.Namespace,
		UID:       bucketAccessRequest.ObjectMeta.UID}
	bucketaccess.Spec.ServiceAccount = &v1.ObjectReference{
		Name:      sa.Name,
		Namespace: sa.Namespace,
		UID:       sa.ObjectMeta.UID}
	// bucketaccess.Spec.MintedSecretName - set by the driver
	bucketaccess.Spec.PolicyActionsConfigMapData, err = util.ReadConfigData(b.kubeClient, bucketAccessClass.PolicyActionsConfigMap)
	if err != nil {
		return err
	}
	//  bucketaccess.Spec.Principal - set by the driver
	bucketaccess.Spec.Provisioner = bucketAccessClass.Provisioner
	bucketaccess.Spec.Parameters = util.CopySS(bucketAccessClass.Parameters)

	bucketaccess, err = b.bucketClient.ObjectstorageV1alpha1().BucketAccesses().Create(context.Background(), bucketaccess, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		bucketAccessRequest.Spec.BucketAccessName = bucketaccess.Name
		_, err := b.bucketClient.ObjectstorageV1alpha1().BucketAccessRequests(bucketAccessRequest.Namespace).Update(ctx, bucketAccessRequest, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	glog.Infof("Finished creating BucketAccess %v", bucketaccess.Name)
	return nil
}

func (b *bucketAccessRequestListener) FindBucketAccess(ctx context.Context, bar *v1alpha1.BucketAccessRequest) *v1alpha1.BucketAccess {
	bucketAccessList, err := b.bucketClient.ObjectstorageV1alpha1().BucketAccesses().List(ctx, metav1.ListOptions{})
	if err != nil || len(bucketAccessList.Items) <= 0 {
		return nil
	}
	for _, bucketaccess := range bucketAccessList.Items {
		if bucketaccess.Spec.BucketAccessRequest.Name == bar.Name &&
			bucketaccess.Spec.BucketAccessRequest.Namespace == bar.Namespace &&
			bucketaccess.Spec.BucketAccessRequest.UID == bar.UID {
			return &bucketaccess
		}
	}
	return nil
}
