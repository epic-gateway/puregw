/*
Copyright 2022 Acnodal.
*/

package gateway

import (
	"context"
	"encoding/json"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	epicgwv1 "acnodal.io/puregw/apis/puregw/v1"
	"acnodal.io/puregw/controllers"
	"acnodal.io/puregw/internal/acnodal"
	"acnodal.io/puregw/internal/gateway"
)

// HTTPRouteReconciler reconciles a HTTPRoute object
type HTTPRouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1a2.HTTPRoute{}).
		Complete(r)
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=puregw.acnodal.io,resources=endpointsliceshadows,verbs=get;list;watch;create;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the HTTPRoute object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	const (
		finalizerName = "epic.acnodal.io/controller"
	)
	var (
		missingService bool = false
		missingParent  bool = false
	)

	// Get the HTTPRoute that triggered this request
	route := gatewayv1a2.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		l.Info("Can't get HTTPRoute, probably deleted", "name", req.NamespacedName)

		// ignore not-found errors, since they can't be fixed by an
		// immediate requeue (we'll need to wait for a new notification),
		// and we can get them on deleted requests.
		return controllers.Done, client.IgnoreNotFound(err)
	}

	// Clean up if this resource is marked for deletion.
	if !route.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("Cleaning up")

		// Remove our finalizer to ensure that we don't block the resource
		// from being deleted.
		if err := controllers.RemoveFinalizer(ctx, r.Client, &route, finalizerName); err != nil {
			l.Error(err, "Removing finalizer")
			// Fall through to delete the EPIC resource
		}

		// Delete the EPIC resource if it was announced.
		if err := maybeDelete(ctx, r.Client, &route); err != nil {
			return controllers.Done, err
		}

		// FIXME: clean up slices. Need to delete the slices that are
		// referenced only by this route

		return controllers.Done, nil
	}

	// Try to get the Gateways to which we refer, and keep trying until
	// we can. If any of the parents is not our GatewayClass then we
	// won't handle this route.
	var config *epicgwv1.GatewayClassConfig
	for _, parent := range route.Spec.ParentRefs {
		gw := gatewayv1a2.Gateway{}
		// FIXME: need to handle multiple parents
		if err := parentGW(ctx, r.Client, parent, &gw); err != nil {
			l.Info("Can't get parent, will retry", "parentRef", parent)
			return controllers.TryAgain, nil
		}

		// See if we're the chosen controller
		var err error
		config, err = getEPICConfig(ctx, r.Client, string(gw.Spec.GatewayClassName))
		if err != nil {
			return controllers.Done, err
		}
		if config == nil {
			l.V(1).Info("Not our ControllerName, will ignore", "parentRef", parent, "controller", gw.Spec.GatewayClassName)
			return controllers.Done, nil
		}
	}

	epic, err := controllers.ConnectToEPIC(ctx, r.Client, &config.Namespace, config.Name)
	if err != nil {
		return controllers.Done, err
	}

	l.V(1).Info("Reconciling")

	account, err := epic.GetAccount()
	if err != nil {
		return controllers.Done, err
	}

	// The resource is not being deleted, and it's our GWClass, so add
	// our finalizer.
	if err := controllers.AddFinalizer(ctx, r.Client, &route, finalizerName); err != nil {
		return controllers.Done, err
	}

	// Prepare the route to be sent to EPIC
	announcedRoute := route.DeepCopy()

	// Munge the ParentRefs so they refer to the Gateways' UIDs, not
	// their names. We use UIDs on the EPIC side because they're unique.
	for i, parent := range announcedRoute.Spec.ParentRefs {
		gw := gatewayv1a2.Gateway{}
		gwName := types.NamespacedName{Namespace: "default", Name: string(parent.Name)}
		if parent.Namespace != nil {
			gwName.Namespace = string(*parent.Namespace)
		}
		if err := r.Get(ctx, gwName, &gw); err != nil {
			l.Info("Parent service not found", "service", gwName)
			missingParent = true
		} else {
			announcedRoute.Spec.ParentRefs[i].Name = gatewayv1a2.ObjectName(gateway.GatewayEPICUID(gw))
		}
	}

	// Munge the ClientRefs so they refer to the services' UIDs, not
	// their names. We use UIDs on the EPIC side because they're
	// unique.
	for i, rule := range announcedRoute.Spec.Rules {
		for j, ref := range rule.BackendRefs {
			svc := corev1.Service{}
			svcName := types.NamespacedName{Namespace: "default", Name: string(ref.Name)}
			if ref.Namespace != nil {
				svcName.Namespace = string(*ref.Namespace)
			}
			if err := r.Get(ctx, svcName, &svc); err != nil {
				l.Info("Referenced service not found", "service", svcName)
				missingService = true
			} else {
				announcedRoute.Spec.Rules[i].BackendRefs[j].Name = gatewayv1a2.ObjectName(svc.UID)
			}
		}
	}

	// See if we've already announced this Route
	if link, announced := route.Annotations[epicgwv1.EPICLinkAnnotation]; announced {

		// The route has been announced, so we might need to either update
		// it or delete it. If we've got complete info about the things to
		// which this route links, then we can update it. If anything is
		// missing then we delete the route, back off, and retry.
		if missingParent || missingService {

			// Delete the EPIC resource.
			l.Info("Previously announced, withdrawing", "link", link)
			if err := maybeDelete(ctx, r.Client, &route); err != nil {
				return controllers.Done, err
			}

			// Remove EPIC annotations so we re-announce when everything is
			// in place.
			if err := removeEpicLink(ctx, r.Client, &route); err != nil {
				return controllers.Done, err
			}

			// Keep retrying until we've got everything that we need to
			// announce.
			return controllers.TryAgain, nil
		} else {
			l.Info("Previously announced, will update", "link", link)
			// Update the Route.
			_, err := epic.UpdateRoute(link,
				acnodal.RouteSpec{
					ClientRef: acnodal.ClientRef{
						Namespace: announcedRoute.Namespace,
						Name:      announcedRoute.Name,
						UID:       string(announcedRoute.UID),
					},
					HTTP: announcedRoute.Spec,
				})
			if err != nil {
				return controllers.Done, err
			}
		}
	} else {
		// If any info is missing then we can't announce so back off and
		// retry later.
		if missingParent || missingService {
			l.Info("Missing info, will back off and retry")
			return controllers.TryAgain, nil
		} else {
			// We have a complete Route so we can announce it and its slices
			// to EPIC.

			// Announce the Slices that the Route references. We do this
			// first so the slices will be in place when the route is
			// announced. The slices need the route to be able to allocate
			// tunnel IDs.
			if err := announceSlices(ctx, r.Client, l, account.Links["create-slice"], epic, config.NamespacedName().String(), &route); err != nil {
				return controllers.Done, err
			}

			// Announce the route to EPIC.
			routeResp, err := epic.AnnounceRoute(account.Links["create-route"],
				acnodal.RouteSpec{
					ClientRef: acnodal.ClientRef{
						Namespace: announcedRoute.Namespace,
						Name:      announcedRoute.Name,
						UID:       string(announcedRoute.UID),
					},
					HTTP: announcedRoute.Spec,
				})
			if err != nil {
				return controllers.Done, err
			}

			// Annotate the Route to mark it as "announced".
			if err := addEpicLink(ctx, r.Client, &route, routeResp.Links["self"], config.NamespacedName().String()); err != nil {
				return controllers.Done, err
			}
			l.Info("Announced", "epic-link", route.Annotations[epicgwv1.EPICLinkAnnotation])
		}
	}

	return controllers.Done, nil
}

// announceSlices announces the slices that this HTTPRoute
// references.If the error return value is non-nil them something has
// gone wrong.
func announceSlices(ctx context.Context, cl client.Client, l logr.Logger, sliceURL string, epic acnodal.EPIC, configName string, route *gatewayv1a2.HTTPRoute) error {
	// Get the set of EndpointSlices that this Route references.
	slices, incomplete, err := routeSlices(ctx, cl, route)
	if err != nil {
		return err
	}
	if incomplete {
		l.Info("Incomplete info, will back off and retry")
		return nil
	}
	l.V(1).Info("Referenced slices", "slices", slices)

	// Announce the EndpointSlices that the Route references.
	for _, slice := range slices {
		// If this slice has been announced then we don't need to do it
		// again. We don't need to update slices - the slice controller
		// will take care of that.
		if hasBeen, _ := hasBeenAnnounced(ctx, cl, slice); hasBeen {
			l.Info("Slice previously announced", "slice", slice.Name)
			continue
		}

		// Build the map of node addresses
		nodeAddrs := map[string]string{}
		for _, ep := range slice.Endpoints {
			node := corev1.Node{}
			nodeName := types.NamespacedName{Namespace: "", Name: *ep.NodeName}
			err := cl.Get(ctx, nodeName, &node)
			if err != nil {
				return err
			}
			for _, addr := range node.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP {
					nodeAddrs[*ep.NodeName] = addr.Address
				}
			}
		}

		// Announce slice
		spec := acnodal.SliceSpec{
			ClientRef: acnodal.ClientRef{
				Namespace: slice.Namespace,
				Name:      slice.Name,
				UID:       string(slice.UID),
			},
			ParentRef: acnodal.ClientRef{
				Namespace: slice.Namespace,
				Name:      slice.ObjectMeta.OwnerReferences[0].Name,
				UID:       string(slice.ObjectMeta.OwnerReferences[0].UID),
			},
			EndpointSlice: *slice,
			NodeAddresses: nodeAddrs,
		}

		// Fix the null endpoints if the service has no replicas. Null
		// endpoints will cause the announcement to fail.
		if spec.EndpointSlice.Endpoints == nil {
			spec.EndpointSlice.Endpoints = []discoveryv1.Endpoint{}
		}

		sliceResp, err := epic.AnnounceSlice(sliceURL, spec)
		if err != nil {
			l.Error(err, "announcing slice")
			continue
		}

		// Annotate the Slice to mark it as "announced".
		if err := addSliceEpicLink(ctx, cl, slice, sliceResp.Links["self"], configName, route); err != nil {
			l.Error(err, "adding EPIC link to slice")
			continue
		}
		l.Info("Slice announced", "epic-link", slice.Annotations[epicgwv1.EPICLinkAnnotation])
	}

	return nil
}

// parentGW gets the parent Gateway resource pointed to by the
// provided ParentRef.
func parentGW(ctx context.Context, cl client.Client, ref gatewayv1a2.ParentRef, gw *gatewayv1a2.Gateway) error {
	gwName := types.NamespacedName{Namespace: "default", Name: string(ref.Name)}
	if ref.Namespace != nil {
		gwName.Namespace = string(*ref.Namespace)
	}
	return cl.Get(ctx, gwName, gw)
}

// routeSlices returns all of the slices that belong to all of
// the services referenced by route. If incomplete is true then
// something is missing so the controller needs to back off and retry
// later. If err is non-nil then the array of EndpointSlices is
// invalid.
func routeSlices(ctx context.Context, cl client.Client, route *gatewayv1a2.HTTPRoute) (slices []*discoveryv1.EndpointSlice, incomplete bool, err error) {
	// Assume that we can reach all of our services.
	incomplete = false

	// Check each rule in the Route.
	for _, rule := range route.Spec.Rules {

		// Get each service that this rule references.
		for _, ref := range rule.BackendRefs {

			// Get the service referenced by this ref.
			svc := corev1.Service{}
			svcName := types.NamespacedName{Namespace: "default", Name: string(ref.Name)}
			if ref.Namespace != nil {
				svcName.Namespace = string(*ref.Namespace)
			}
			err = cl.Get(ctx, svcName, &svc)
			if err != nil {
				// If the service doesn't exist yet then tell the controller
				// to back off and retry.
				if apierrors.IsNotFound(err) {
					incomplete = true
				} else {
					// If it's some other sort of error then tell the controller.
					return
				}
			}

			// Get the slices that belong to this service.
			sliceList := discoveryv1.EndpointSliceList{}
			if err = cl.List(ctx, &sliceList, &client.ListOptions{
				Namespace: route.Namespace,
				LabelSelector: labels.SelectorFromSet(map[string]string{
					"kubernetes.io/service-name": svc.Name,
				}),
			}); err != nil {
				return
			}

			// Add each slice to the return array.
			for _, slice := range sliceList.Items {
				slices = append(slices, &slice)
			}
		}
	}

	return
}

// addEpicLink adds our annotations that indicate that the route has
// been announced.
func addEpicLink(ctx context.Context, cl client.Client, route *gatewayv1a2.HTTPRoute, link string, configName string) error {
	var (
		patch      []map[string]interface{}
		patchBytes []byte
		err        error
	)

	if route.Annotations == nil {
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
	if err := cl.Patch(ctx, route, client.RawPatch(types.JSONPatchType, patchBytes)); err != nil {
		return err
	}

	return nil
}

// removeEpicLink removes our annotations that indicate that the route
// has been announced.
func removeEpicLink(ctx context.Context, cl client.Client, route *gatewayv1a2.HTTPRoute) error {
	var (
		patch      []map[string]interface{}
		patchBytes []byte
		err        error
	)

	// Remove our annotations, if present.
	for annKey := range route.Annotations {
		if annKey == epicgwv1.EPICLinkAnnotation {
			patch = append(patch, map[string]interface{}{
				"op":   "remove",
				"path": epicgwv1.EPICLinkAnnotationPatch,
			})
		} else if annKey == epicgwv1.EPICConfigAnnotation {
			patch = append(patch, map[string]interface{}{
				"op":   "remove",
				"path": epicgwv1.EPICConfigAnnotationPatch,
			})
		}
	}

	// apply the patch
	if patchBytes, err = json.Marshal(patch); err != nil {
		return err
	}
	if err := cl.Patch(ctx, route, client.RawPatch(types.JSONPatchType, patchBytes)); err != nil {
		return err
	}

	return nil
}

// addSliceEpicLink adds our annotations that indicate that the slice
// has been announced.
func addSliceEpicLink(ctx context.Context, cl client.Client, slice *discoveryv1.EndpointSlice, link string, configName string, route *gatewayv1a2.HTTPRoute) error {
	kind := gatewayv1a2.Kind("HTTPRoute")
	ns := gatewayv1a2.Namespace(route.Namespace)
	name := gatewayv1a2.ObjectName(route.Name)

	shadow := epicgwv1.EndpointSliceShadow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      slice.Name,
			Namespace: slice.Namespace,
		},
		Spec: epicgwv1.EndpointSliceShadowSpec{
			EPICConfigName: configName,
			EPICLink:       link,
			ParentRoutes: []gatewayv1a2.ParentRef{{
				Kind:      &kind,
				Namespace: &ns,
				Name:      name,
			}},
		},
	}

	return cl.Create(ctx, &shadow)
}

func hasBeenAnnounced(ctx context.Context, cl client.Client, slice *discoveryv1.EndpointSlice) (bool, error) {
	shadow := epicgwv1.EndpointSliceShadow{}
	name := types.NamespacedName{Namespace: slice.Namespace, Name: slice.Name}
	if err := cl.Get(ctx, name, &shadow); err != nil {
		return false, err
	}
	return true, nil
}

func maybeDelete(ctx context.Context, cl client.Client, route *gatewayv1a2.HTTPRoute) error {
	link, announced := route.Annotations[epicgwv1.EPICLinkAnnotation]
	if announced {
		// Get cached config name
		configName, err := controllers.SplitNSName(route.Annotations[epicgwv1.EPICConfigAnnotation])
		if err != nil {
			return err
		}
		epic, err := controllers.ConnectToEPIC(ctx, cl, &configName.Namespace, configName.Name)
		if err != nil {
			return err
		}
		err = epic.Delete(link)
		if err != nil {
			return err
		}
	}

	return nil
}
