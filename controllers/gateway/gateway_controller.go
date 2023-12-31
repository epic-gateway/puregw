/*
Copyright 2022 Acnodal.
*/

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	epicgwv1 "epic-gateway.org/puregw/apis/puregw/v1"
	"epic-gateway.org/puregw/controllers"
	"epic-gateway.org/puregw/internal/contour/dag"
	"epic-gateway.org/puregw/internal/contour/gatewayapi"
	"epic-gateway.org/puregw/internal/contour/status"
	"epic-gateway.org/puregw/internal/gateway"
)

// GatewayReconciler reconciles a Gateway object
type GatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1a2.Gateway{}).
		Complete(r)
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencepolicies,verbs=get;list;watch
//+kubebuilder:rbac:groups=puregw.epic-gateway.org,resources=gatewayclassconfigs,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Gateway object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// Get the Gateway that caused this request
	gw := gatewayv1a2.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		// ignore not-found errors, since they can't be fixed by an
		// immediate requeue (we'll need to wait for a new notification),
		// and we can get them on deleted requests.
		return controllers.Done, client.IgnoreNotFound(err)
	}

	config, err := getEPICConfig(ctx, r.Client, string(gw.Spec.GatewayClassName))
	if err != nil {
		return controllers.Done, err
	}
	if config == nil {
		l.V(1).Info("Not our ControllerName, will ignore")
		return controllers.Done, nil
	}

	epic, err := controllers.ConnectToEPIC(ctx, r.Client, &config.Namespace, config.Name)
	if err != nil {
		return controllers.Done, err
	}

	if !gw.ObjectMeta.DeletionTimestamp.IsZero() {
		// This resource is marked for deletion.
		l.Info("Cleaning up")

		// Remove our finalizer to ensure that we don't block the resource
		// from being deleted.
		if err := controllers.RemoveFinalizer(ctx, r.Client, &gw, controllers.FinalizerName); err != nil {
			l.Error(err, "Removing finalizer")
			// Fall through to delete the EPIC resource
		}

		// Delete the EPIC resource
		link, announced := gw.Annotations[epicgwv1.EPICLinkAnnotation]
		if announced {
			err = epic.Delete(link)
			if err != nil {
				return controllers.Done, err
			}
		}

		return controllers.Done, nil
	}

	// The resource is not being deleted, and it's our GWClass, so add
	// our finalizer.
	if err := controllers.AddFinalizer(ctx, r.Client, &gw, controllers.FinalizerName); err != nil {
		return controllers.Done, err
	}

	// See if we've already announced this resource.
	link, announced := gw.Annotations[epicgwv1.EPICLinkAnnotation]
	if announced {
		l.Info("Previously announced", "link", link)
		return controllers.Done, nil
	}

	l.V(1).Info("Reconciling")

	gsu := status.GatewayStatusUpdate{
		FullName:           types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name},
		Conditions:         make(map[gatewayv1a2.GatewayConditionType]metav1.Condition),
		ExistingConditions: nil,
		Generation:         gw.Generation,
		TransitionTime:     metav1.NewTime(time.Now()),
	}

	// Set the listener supportedKinds
	for _, listener := range gw.Spec.Listeners {
		gsu.SetListenerSupportedKinds(string(listener.Name), epicgwv1.SupportedKinds)
	}

	// Validate TLS configuration (if present)
	tlsOK := true
	for _, listener := range gw.Spec.Listeners {
		if listener.TLS != nil {
			// If we have a TLS config then we need to validate it.
			if dag.ValidGatewayTLS(gw, *listener.TLS, string(listener.Name), &gsu, r) == nil {
				tlsOK = false
			}
		}
	}

	// If there's something wrong with the TLS config then mark the
	// gateway and don't announce.
	if !tlsOK {
		gsu.AddCondition(gatewayv1a2.GatewayConditionScheduled, metav1.ConditionFalse, status.ReasonValidGateway, "Invalid GatewayTLS")
		gsu.AddCondition(gatewayv1a2.GatewayConditionReady, metav1.ConditionTrue, status.ReasonValidGateway, "Not announced to EPIC")
		if err := updateStatus(ctx, r.Client, l, &gw, &gsu); err != nil {
			return controllers.Done, err
		}
		return controllers.Done, nil
	}

	// Get the EPIC ServiceGroup
	group, err := epic.GetGroup()
	if err != nil {
		return controllers.Done, err
	}

	// Announce the Gateway
	response, err := epic.AnnounceGateway(group.Links["create-proxy"], gw)
	if err != nil {
		// Tell the user that something has gone wrong
		gsu.AddCondition(gatewayv1a2.GatewayConditionScheduled, metav1.ConditionFalse, status.ReasonValidGateway, err.Error())
		updateStatus(ctx, r.Client, l, &gw, &gsu)
		return controllers.Done, err
	}

	// Annotate the Gateway with its URL to mark it as "announced".
	if err := r.addEpicLink(ctx, &gw, response.Links["self"], config.NamespacedName().String()); err != nil {
		return controllers.Done, err
	}
	l.Info("Announced", "self-link", response.Links["self"])

	// Add the allocated IP and hostname to the GW status.
	if err := markAddresses(ctx, r.Client, l, &gw, response.Gateway.Spec.Address, response.Gateway.Spec.Endpoints[0].DNSName); err != nil {
		return controllers.Done, err
	}

	// Tell the user that we're working on bringing up the Gateway.
	gsu.AddCondition(gatewayv1a2.GatewayConditionReady, metav1.ConditionTrue, status.ReasonValidGateway, "Announced to EPIC")
	if err := updateStatus(ctx, r.Client, l, &gw, &gsu); err != nil {
		return controllers.Done, err
	}

	// Nudge this GW's HTTPRoutes. Here's the scenario: create a GW and
	// some children. Everything works. Now delete the GW and re-load
	// it. The new GW has a different UID so the EPIC GWRoutes won't
	// link to the new GWGateway. We need to nudge the children so they
	// send an update to EPIC that links to the new GWRoute.
	children := []gatewayv1a2.HTTPRoute{}
	if children, err = gatewayChildren(ctx, r.Client, l, &gw); err != nil {
		return controllers.Done, err
	}
	l.Info("Nudging children", "childCount", len(children))
	for _, route := range children {
		if err = gateway.Nudge(ctx, r.Client, l, &route); err != nil {
			l.Error(err, "Nudging HTTPRoute", "gateway", gw, "route", route)
		}
	}

	return controllers.Done, nil
}

// Cleanup removes our finalizer from all of the Gateways in the
// system.
func (r *GatewayReconciler) Cleanup(l logr.Logger, ctx context.Context) error {
	gwList := gatewayv1a2.GatewayList{}
	if err := r.Client.List(ctx, &gwList); err != nil {
		return err
	}
	for _, route := range gwList.Items {
		if err := controllers.RemoveFinalizer(ctx, r.Client, &route, controllers.FinalizerName); err != nil {
			l.Error(err, "removing Finalizer")
		}
	}
	return nil
}

func getEPICConfig(ctx context.Context, cl client.Client, gatewayClassName string) (*epicgwv1.GatewayClassConfig, error) {
	// Get the owning GatewayClass
	gc := gatewayv1a2.GatewayClass{}
	if err := cl.Get(ctx, types.NamespacedName{Name: gatewayClassName}, &gc); err != nil {
		return nil, fmt.Errorf("Unable to get GatewayClass %s", gatewayClassName)
	}

	// Check controller name - are we the right controller?
	if gc.Spec.ControllerName != controllers.GatewayController {
		return nil, nil
	}

	// Get the PureGW GatewayClassConfig referred to by the GatewayClass
	gwcName := types.NamespacedName{Namespace: "default", Name: string(gc.Spec.ParametersRef.Name)}
	if gc.Spec.ParametersRef.Namespace != nil {
		gwcName.Namespace = string(*gc.Spec.ParametersRef.Namespace)
	}
	gwc := epicgwv1.GatewayClassConfig{}
	if err := cl.Get(ctx, gwcName, &gwc); err != nil {
		return nil, fmt.Errorf("Unable to get GatewayClassConfig %s", gwcName)
	}

	return &gwc, nil
}

// addEpicLink adds an EPICLinkAnnotation annotation to gw.
func (r *GatewayReconciler) addEpicLink(ctx context.Context, gw *gatewayv1a2.Gateway, link string, configName string) error {
	var (
		patch      []map[string]interface{}
		patchBytes []byte
		err        error
	)

	if gw.Annotations == nil {
		// If this is the first annotation then we need to wrap it in an
		// object
		patch = []map[string]interface{}{{
			"op":   "add",
			"path": "/metadata/annotations",
			"value": map[string]string{
				epicgwv1.EPICLinkAnnotation:   link,
				epicgwv1.EPICConfigAnnotation: configName,
			},
		}}
	} else {
		// If there are other annotations then we can just add this one
		patch = []map[string]interface{}{
			{
				"op":    "add",
				"path":  epicgwv1.EPICLinkAnnotationPatch,
				"value": link,
			},
			{
				"op":    "add",
				"path":  epicgwv1.EPICConfigAnnotationPatch,
				"value": configName,
			},
		}
	}

	// apply the patch
	if patchBytes, err = json.Marshal(patch); err != nil {
		return err
	}
	if err := r.Patch(ctx, gw, client.RawPatch(types.JSONPatchType, patchBytes)); err != nil {
		return err
	}

	return nil
}

// GetSecret implements the dag.Fetcher GetSecret() method.
// FIXME: move this into its own class so we can use the correct context.
func (r *GatewayReconciler) GetSecret(name types.NamespacedName) (*v1.Secret, error) {
	secret := v1.Secret{}
	return &secret, r.Get(context.Background(), name, &secret)
}

// GetGrants implements the dag.Fetcher GetGrants() method.
func (r *GatewayReconciler) GetGrants(ns string) (gatewayv1a2.ReferencePolicyList, error) {
	classList := gatewayv1a2.ReferencePolicyList{}
	return classList, r.List(context.Background(), &classList)
}

// updateStatus uses Contour's GatewayStatusUpdate to update the
// Gateway's status.
func updateStatus(ctx context.Context, cl client.Client, l logr.Logger, gw *gatewayv1a2.Gateway, gsu *status.GatewayStatusUpdate) error {
	key := client.ObjectKey{Namespace: gw.GetNamespace(), Name: gw.GetName()}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch the resource here; you need to refetch it on every try,
		// since if you got a conflict on the last update attempt then
		// you need to get the current version before making your own
		// changes.
		if err := cl.Get(ctx, key, gw); err != nil {
			return err
		}

		got, ok := gsu.Mutate(gw).(*gatewayv1a2.Gateway)
		if !ok {
			return fmt.Errorf("Failed to mutate Gateway")
		}

		// Try to update
		return cl.Status().Update(ctx, got)
	})
}

// markAddresses adds publicIP and publicHostname to gw's status.
func markAddresses(ctx context.Context, cl client.Client, l logr.Logger, gw *gatewayv1a2.Gateway, publicIP string, publicHostname string) error {
	key := client.ObjectKey{Namespace: gw.GetNamespace(), Name: gw.GetName()}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch the resource here; you need to refetch it on every try,
		// since if you got a conflict on the last update attempt then
		// you need to get the current version before making your own
		// changes.
		if err := cl.Get(ctx, key, gw); err != nil {
			return err
		}

		// Add the IP address to GW.Status so the user can find out what
		// it is.
		gw.Status.Addresses = []gatewayv1a2.GatewayAddress{{
			Type:  gatewayapi.AddressTypePtr(gatewayv1a2.IPAddressType),
			Value: publicIP,
		}}
		if publicHostname != "" {
			gw.Status.Addresses = append(gw.Status.Addresses, gatewayv1a2.GatewayAddress{
				Type:  gatewayapi.AddressTypePtr(gatewayv1a2.HostnameAddressType),
				Value: publicHostname,
			})
		}

		// Try to update
		return cl.Status().Update(ctx, gw)
	})
}

// gatewayChildren finds the HTTPRoutes that refer to gw. It's a
// brute-force approach but will likely work up to 1000's of routes.
func gatewayChildren(ctx context.Context, cl client.Client, l logr.Logger, gw *gatewayv1a2.Gateway) (routes []gatewayv1a2.HTTPRoute, err error) {
	// FIXME: This will not scale, but that may be OK. I expect that
	// most systems will have no more than a few dozen routes so
	// iterating over all of them is probably OK.

	// Get the routes that belong to this service.
	routeList := gatewayv1a2.HTTPRouteList{}
	if err = cl.List(ctx, &routeList, &client.ListOptions{Namespace: ""}); err != nil {
		return
	}
	l.Info("Child candidates", "count", len(routeList.Items))

	gwName := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
	for _, route := range routeList.Items {
		for _, ref := range route.Spec.ParentRefs {
			l.Info("*** Comparing", "gw", gwName, "ref", ref)

			if isRefToGateway(ref, gwName) {
				routes = append(routes, route)
			}
		}
	}

	return
}

// isRefToGateway returns whether or not ref is a reference
// to a Gateway with the given namespace & name.
func isRefToGateway(ref gatewayv1a2.ParentReference, gateway types.NamespacedName) bool {
	// This is copied from internal/status/routeconditions.go which
	// doesn't seem to handle "default" as a namespace.
	if ref.Group != nil && *ref.Group != gatewayv1a2.GroupName {
		return false
	}

	if ref.Kind != nil && *ref.Kind != "Gateway" {
		return false
	}

	if ref.Namespace == nil {
		if gateway.Namespace != "default" {
			return false
		}
	} else {
		if *ref.Namespace != gatewayv1a2.Namespace(gateway.Namespace) {
			return false
		}
	}

	return string(ref.Name) == gateway.Name
}
