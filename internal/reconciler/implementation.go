package reconciler

import (
	"context"
	"fmt"
	"reflect"

	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nginxinc/nginx-kubernetes-gateway/internal/events"
)

// NamespacedNameFilterFunc is a function that returns true if the resource should be processed by the reconciler.
// If the function returns false, the reconciler will log the returned string.
type NamespacedNameFilterFunc func(nsname types.NamespacedName) (bool, string)

// ValidatorFunc validates a Kubernetes resource.
type ValidatorFunc func(object client.Object) error

// Config contains the configuration for the Implementation.
type Config struct {
	// Getter gets a resource from the k8s API.
	Getter Getter
	// ObjectType is the type of the resource that the reconciler will reconcile.
	ObjectType client.Object
	// EventCh is the channel where the reconciler will send events.
	EventCh chan<- interface{}
	// NamespacedNameFilter filters resources the controller will process. Can be nil.
	NamespacedNameFilter NamespacedNameFilterFunc
	// WebhookValidator validates a resource using the same rules as in the Gateway API Webhook. Can be nil.
	WebhookValidator ValidatorFunc
	// EventRecorder records event about resources.
	EventRecorder EventRecorder
}

// Implementation is a reconciler for Kubernetes resources.
// It implements the reconcile.Reconciler interface.
// A successful reconciliation of a resource has the two possible outcomes:
// (1) If the resource is deleted, the Implementation will send a DeleteEvent to the event channel.
// (2) If the resource is upserted (created or updated), the Implementation will send an UpsertEvent
// to the event channel.
type Implementation struct {
	cfg Config
}

var _ reconcile.Reconciler = &Implementation{}

// NewImplementation creates a new Implementation.
func NewImplementation(cfg Config) *Implementation {
	return &Implementation{
		cfg: cfg,
	}
}

func newObject(objectType client.Object) client.Object {
	// without Elem(), t will be a pointer to the type. For example, *v1beta1.Gateway, not v1beta1.Gateway
	t := reflect.TypeOf(objectType).Elem()

	// We could've used objectType.DeepCopyObject() here, but it's a bit slower confirmed by benchmarks.

	return reflect.New(t).Interface().(client.Object)
}

const (
	webhookValidationErrorLogMsg = "Rejected the resource because the Gateway API webhook failed to reject it with " +
		"a validation error; make sure the webhook is installed and running correctly; " +
		"NKG will delete any existing NGINX configuration that corresponds to the resource"
)

// Reconcile implements the reconcile.Reconciler Reconcile method.
func (r *Implementation) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	// The controller runtime has set the logger with the group, kind, namespace and name of the resource,
	// and a few other key/value pairs. So we don't need to set them here.

	logger.Info("Reconciling the resource")

	if r.cfg.NamespacedNameFilter != nil {
		if allow, msg := r.cfg.NamespacedNameFilter(req.NamespacedName); !allow {
			logger.Info(msg)
			return reconcile.Result{}, nil
		}
	}

	obj := newObject(r.cfg.ObjectType)
	err := r.cfg.Getter.Get(ctx, req.NamespacedName, obj)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to get the resource")
			return reconcile.Result{}, err
		}
		// The resource does not exist (was deleted).
		obj = nil
	}

	var validationError error
	if obj != nil && r.cfg.WebhookValidator != nil {
		validationError = r.cfg.WebhookValidator(obj)
	}

	if validationError != nil {
		logger.Error(validationError, webhookValidationErrorLogMsg)
		r.cfg.EventRecorder.Eventf(obj, apiv1.EventTypeWarning, "Rejected",
			webhookValidationErrorLogMsg+"; validation error: %v", validationError)
	}

	var e interface{}
	var op string

	if obj == nil || validationError != nil {
		// In case of a validation error, we handle the resource as if it was deleted.
		e = &events.DeleteEvent{
			Type:           r.cfg.ObjectType,
			NamespacedName: req.NamespacedName,
		}
		op = "Deleted"
	} else {
		e = &events.UpsertEvent{
			Resource: obj,
		}
		op = "Upserted"
	}

	select {
	case <-ctx.Done():
		logger.Info("Did not process the resource because the context was canceled")
		return reconcile.Result{}, nil
	case r.cfg.EventCh <- e:
	}

	logger.Info(fmt.Sprintf("%s the resource", op))

	return reconcile.Result{}, nil
}
