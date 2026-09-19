package resources

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

var (
	ErrNoPorts            = errors.New("resources: webapp declares no container ports")
	ErrUnknownIngressPort = errors.New("resources: spec.ingressPortName names no declared port")
)

const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	NameLabel      = "app.kubernetes.io/name"
	InstanceLabel  = "app.kubernetes.io/instance"
	ManagedByValue = "webapp-operator"

	DefaultPortName = "http"
)

func SelectorLabels(app *v1alpha1.WebApp) map[string]string {
	return map[string]string{
		NameLabel:      app.Name,
		InstanceLabel:  app.Name,
		ManagedByLabel: ManagedByValue,
	}
}

func Labels(app *v1alpha1.WebApp) map[string]string {
	return SelectorLabels(app)
}

func portName(p v1alpha1.ContainerPort) string {
	if p.Name != "" {
		return p.Name
	}
	if protocol(p) == corev1.ProtocolUDP {
		return fmt.Sprintf("port-%d-udp", p.ContainerPort)
	}
	return fmt.Sprintf("port-%d", p.ContainerPort)
}

func protocol(p v1alpha1.ContainerPort) corev1.Protocol {
	if p.Protocol == "" {
		return corev1.ProtocolTCP
	}
	return p.Protocol
}

type portKey struct {
	port     int32
	protocol corev1.Protocol
}

func allPorts(app *v1alpha1.WebApp) []v1alpha1.ContainerPort {
	var out []v1alpha1.ContainerPort
	seen := map[portKey]bool{}
	for _, c := range app.Spec.Containers {
		for _, p := range c.Ports {
			k := portKey{p.ContainerPort, protocol(p)}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

func PortNames(app *v1alpha1.WebApp) []string {
	ports := allPorts(app)
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, portName(p))
	}
	return out
}

func backendPort(app *v1alpha1.WebApp) (string, error) {
	ports := allPorts(app)
	if len(ports) == 0 {
		return "", ErrNoPorts
	}
	if want := app.Spec.IngressPortName; want != "" {
		for _, p := range ports {
			if portName(p) == want {
				return want, nil
			}
		}
		return "", fmt.Errorf("%w: %q", ErrUnknownIngressPort, want)
	}
	for _, p := range ports {
		if p.Name == DefaultPortName {
			return portName(p), nil
		}
	}
	return portName(ports[0]), nil
}

func OwnerRefs(app *v1alpha1.WebApp) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "WebApp",
		Name:               app.Name,
		UID:                app.UID,
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}}
}
