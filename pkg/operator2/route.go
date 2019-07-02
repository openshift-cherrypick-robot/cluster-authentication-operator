package operator2

import (
	"crypto/x509"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog"

	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
)

func (c *authOperator) handleRoute(ingress *configv1.Ingress) (*routev1.Route, *corev1.Secret, error) {
	route, err := c.route.Get(targetName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		route, err = c.route.Create(defaultRoute(ingress))
	}
	if err != nil {
		return nil, nil, err
	}

	host := getCanonicalHost(route, ingress)
	if len(host) == 0 {
		// be careful not to print route.spec as it many contain secrets
		return nil, nil, fmt.Errorf("route is not available at canonical host %s: %+v", ingressToHost(ingress), route.Status.Ingress)
	}

	// assume it is unsafe to mutate route in case we go to a shared informer in the future
	// this way everything else can just assume route.Spec.Host is correct
	// note that we are not updating route.Spec.Host in the API - that value is nonsense to us
	route = route.DeepCopy()
	route.Spec.Host = host

	if err := isValidRoute(route, ingress); err != nil {
		// TODO remove this delete so that we do not lose the early creation timestamp of our route
		// delete the route so that it is replaced with the proper one in next reconcile loop
		klog.Infof("deleting invalid route: %#v", route)
		opts := &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &route.UID}}
		if err := c.route.Delete(route.Name, opts); err != nil && !errors.IsNotFound(err) {
			klog.Infof("failed to delete invalid route: %v", err)
		}
		return nil, nil, err
	}

	routerSecret, err := c.secrets.Secrets(targetNamespace).Get(routerCertsLocalName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	if len(routerSecret.Data) == 0 {
		// be careful not to print the routerSecret even when it is empty
		return nil, nil, fmt.Errorf("router secret %s/%s is empty", routerSecret.Namespace, routerSecret.Name)
	}

	return route, routerSecret, nil
}

func isValidRoute(route *routev1.Route, ingress *configv1.Ingress) error {
	// TODO: return all errors at once
	// TODO error when fields that should be empty are set

	// get the expected settings from the default route
	expectedRoute := defaultRoute(ingress)
	expName := expectedRoute.Spec.To.Name
	expPort := expectedRoute.Spec.Port.TargetPort.IntValue()
	expTLSTermination := expectedRoute.Spec.TLS.Termination
	expInsecureEdgeTerminationPolicy := expectedRoute.Spec.TLS.InsecureEdgeTerminationPolicy

	if route.Spec.To.Name != expName {
		return fmt.Errorf("route targets a wrong service - needs %s: %#v", expName, route)
	}

	if route.Spec.Port.TargetPort.IntValue() != expPort {
		return fmt.Errorf("expected port '%d' for route: %#v", expPort, route)
	}

	if route.Spec.TLS == nil {
		return fmt.Errorf("TLS needs to be configured for route: %#v", route)
	}

	if route.Spec.TLS.Termination != expTLSTermination {
		return fmt.Errorf("route contains wrong TLS termination - '%s' is required: %#v", expTLSTermination, route)
	}

	if route.Spec.TLS.InsecureEdgeTerminationPolicy != expInsecureEdgeTerminationPolicy {
		return fmt.Errorf("route contains wrong insecure termination policy - '%s' is required: %#v", expInsecureEdgeTerminationPolicy, route)
	}

	return nil
}

func defaultRoute(ingress *configv1.Ingress) *routev1.Route {
	return &routev1.Route{
		ObjectMeta: defaultMeta(),
		Spec: routev1.RouteSpec{
			Host:      ingressToHost(ingress), // mimic the behavior of subdomain
			Subdomain: "",                     // TODO once subdomain is functional, remove reliance on ingress config and just set subdomain=targetName
			To: routev1.RouteTargetReference{
				Kind: "Service",
				Name: targetName,
			},
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromInt(containerPort),
			},
			TLS: &routev1.TLSConfig{
				Termination:                   routev1.TLSTerminationPassthrough,
				InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
			},
		},
	}
}

func routerSecretToSNI(routerSecret *corev1.Secret) []configv1.NamedCertificate {
	var out []configv1.NamedCertificate
	for domain := range routerSecret.Data {
		out = append(out, configv1.NamedCertificate{
			Names: []string{"*." + domain}, // ingress domain is always a wildcard
			CertInfo: configv1.CertInfo{ // the cert and key are appended together
				CertFile: routerCertsLocalMount + "/" + domain,
				KeyFile:  routerCertsLocalMount + "/" + domain,
			},
		})
	}
	return out
}

func routerSecretToCA(route *routev1.Route, routerSecret *corev1.Secret, ingress *configv1.Ingress) []byte {
	var caData []byte

	// find the domain that matches our route
	if certs, ok := routerSecret.Data[ingress.Spec.Domain]; ok {
		caData = certs
	}

	// if we have no CA, use system roots (or more correctly, if we have no CERTIFICATE block)
	// TODO so this branch is effectively never taken, because the value of caData
	// is the concatenation of tls.crt and tls.key - the .crt data gets parsed
	// as a valid cert by AppendCertsFromPEM meaning ok is always true.
	// because Go is weird with how it validates TLS connections, having the actual
	// peer cert loaded in the transport is totally fine with the connection even
	// without having the CA loaded.  this is weird but it lets us tolerate scenarios
	// where we do not have the CA (i.e. admin is using a cert from an internal company CA).
	// thus the only way we take this branch is if len(caData) == 0
	if ok := x509.NewCertPool().AppendCertsFromPEM(caData); !ok {
		klog.Infof("using global CAs for %s, ingress domain=%s, cert data len=%d", route.Spec.Host, ingress.Spec.Domain, len(caData))
		return nil
	}

	return caData
}

func getCanonicalHost(route *routev1.Route, ingressConfig *configv1.Ingress) string {
	host := ingressToHost(ingressConfig)
	for _, ingress := range route.Status.Ingress {
		if ingress.Host != host {
			continue
		}
		if !isIngressAdmitted(ingress) {
			continue
		}
		return host
	}
	return ""
}

func isIngressAdmitted(ingress routev1.RouteIngress) bool {
	for _, condition := range ingress.Conditions {
		if condition.Type == routev1.RouteAdmitted && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func ingressToHost(ingress *configv1.Ingress) string {
	return targetName + "." + ingress.Spec.Domain
}
