package integration_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

// These assert the rules the CRD schema enforces on its own. Schema validation
// runs before validating admission, so a rejection carrying the CEL or pattern
// message proves the rule still holds with -enable-webhooks=false.
func rejects(t *testing.T, app *v1alpha1.WebApp, want string) {
	t.Helper()
	err := k8sClient.Create(testCtx, app)
	if err == nil {
		t.Fatalf("want rejection mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("want an error mentioning %q, got %v", want, err)
	}
	if strings.Contains(err.Error(), "denied the request") {
		t.Fatalf("want the schema to reject this without the webhook, got %v", err)
	}
}

func TestSchema_RejectsPortNameWithoutALetter(t *testing.T) {
	app := baseWebApp(newNamespace(t), "digit-port")
	app.Spec.Containers[0].Ports[0].Name = "8080"
	rejects(t, app, "at least one letter")
}

func TestSchema_RejectsConsecutiveHyphensInPortName(t *testing.T) {
	app := baseWebApp(newNamespace(t), "hyphen-port")
	app.Spec.Containers[0].Ports[0].Name = "a--b"
	rejects(t, app, "spec.containers[0].ports[0].name")
}

func TestSchema_RejectsAutoscalingWithoutMaxReplicas(t *testing.T) {
	app := baseWebApp(newNamespace(t), "schema-no-max")
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true}
	rejects(t, app, "maxReplicas is required")
}

func TestSchema_RejectsMaxBelowMin(t *testing.T) {
	app := baseWebApp(newNamespace(t), "schema-bounds")
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:     true,
		MinReplicas: new(int32(5)),
		MaxReplicas: new(int32(2)),
	}
	rejects(t, app, "greater than or equal to minReplicas")
}

func TestSchema_RejectsRecreateWithRollingUpdate(t *testing.T) {
	app := baseWebApp(newNamespace(t), "schema-strategy")
	app.Spec.Strategy = &v1alpha1.StrategySpec{
		Type:          appsv1.RecreateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{},
	}
	rejects(t, app, "may only be set when strategy.type is RollingUpdate")
}

func TestSchema_RejectsInvalidDomain(t *testing.T) {
	app := baseWebApp(newNamespace(t), "schema-domain")
	app.Spec.Domain = "Not_A_Domain"
	rejects(t, app, "spec.domain")
}

func TestSchema_RejectsIngressClassNameWithoutDomain(t *testing.T) {
	app := baseWebApp(newNamespace(t), "orphan-class")
	app.Spec.IngressClassName = new("nginx")
	rejects(t, app, "ingressClassName is only meaningful with spec.domain")
}

func TestSchema_RejectsIngressPortNameWithoutDomain(t *testing.T) {
	app := baseWebApp(newNamespace(t), "orphan-port")
	app.Spec.IngressPortName = "http"
	rejects(t, app, "ingressPortName is only meaningful with spec.domain")
}

func TestSchema_RejectsNonDNSContainerName(t *testing.T) {
	app := baseWebApp(newNamespace(t), "bad-container-name")
	app.Spec.Containers[0].Name = "Web_Server"
	rejects(t, app, "spec.containers[0].name")
}

func TestSchema_RejectsRootUID(t *testing.T) {
	app := baseWebApp(newNamespace(t), "root-uid")
	app.Spec.SecurityContext = &v1alpha1.PodSecurityContext{RunAsUser: new(int64(0))}
	rejects(t, app, "runAsUser")
}

func TestSchema_AllowsTargetUtilizationAbove100(t *testing.T) {
	app := autoscale(baseWebApp(newNamespace(t), "burst-target"), 1, 5)
	app.Spec.Autoscaling.TargetCPUUtilizationPercentage = new(int32(250))
	if err := k8sClient.Create(testCtx, app); err != nil {
		t.Fatalf("utilization is a ratio of requests, so >100 must be allowed: %v", err)
	}
}

func TestSchema_RejectsReservedPodLabelKey(t *testing.T) {
	app := baseWebApp(newNamespace(t), "reserved-label")
	app.Spec.PodLabels = map[string]string{"app.kubernetes.io/name": "hijacked"}
	rejects(t, app, "are reserved")
}

func TestSchema_RejectsPodTemplateHashLabel(t *testing.T) {
	app := baseWebApp(newNamespace(t), "template-hash")
	app.Spec.PodLabels = map[string]string{"pod-template-hash": "abc123"}
	rejects(t, app, "are reserved")
}

func TestSchema_AllowsOrdinaryPodLabels(t *testing.T) {
	app := baseWebApp(newNamespace(t), "ok-labels")
	app.Spec.PodLabels = map[string]string{
		"team": "payments",
		// Recommended labels the operator does not own must stay usable.
		"app.kubernetes.io/version":   "1.4.2",
		"app.kubernetes.io/component": "frontend",
	}
	app.Spec.PodAnnotations = map[string]string{"prometheus.io/scrape": "true"}
	if err := k8sClient.Create(testCtx, app); err != nil {
		t.Fatalf("ordinary pod metadata must be allowed: %v", err)
	}
}

func TestSchema_RejectsRelativeScratchMountPath(t *testing.T) {
	app := baseWebApp(newNamespace(t), "relative-mount")
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
		{Name: "tmp", MountPath: "tmp"},
	}
	rejects(t, app, "scratchVolumes")
}

func TestSchema_RejectsReservedScratchMountPaths(t *testing.T) {
	for _, path := range []string{"/", "/proc", "/sys", "/dev", "/etc/ssl", "/var/run/secrets/kubernetes.io"} {
		app := baseWebApp(newNamespace(t), "reserved-mount")
		app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
			{Name: "v", MountPath: path},
		}
		err := k8sClient.Create(testCtx, app)
		if err == nil {
			t.Fatalf("mountPath %q must be rejected", path)
		}
		if !strings.Contains(err.Error(), "reserved mount path") {
			t.Fatalf("mountPath %q: unexpected error %v", path, err)
		}
	}
}

func TestSchema_AllowsOrdinaryScratchMountPaths(t *testing.T) {
	for _, path := range []string{"/tmp", "/var/cache", "/etcd-data", "/devices"} {
		app := baseWebApp(newNamespace(t), "ok-mount")
		app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
			{Name: "v", MountPath: path},
		}
		if err := k8sClient.Create(testCtx, app); err != nil {
			t.Fatalf("mountPath %q must be allowed: %v", path, err)
		}
	}
}

func TestSchema_RejectsDuplicateScratchMountPath(t *testing.T) {
	app := baseWebApp(newNamespace(t), "dup-mount")
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
		{Name: "one", MountPath: "/tmp"},
		{Name: "two", MountPath: "/tmp"},
	}
	rejects(t, app, "mountPath must be unique")
}

func TestSchema_RequiresSizeLimitForMemoryMedium(t *testing.T) {
	app := baseWebApp(newNamespace(t), "unbounded-tmpfs")
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
		{Name: "tmp", MountPath: "/tmp", Medium: v1alpha1.ScratchMediumMemory},
	}
	rejects(t, app, "sizeLimit is required when medium is Memory")
}

func TestSchema_DefaultsScratchMediumToDisk(t *testing.T) {
	app := baseWebApp(newNamespace(t), "scratch-default")
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
		{Name: "tmp", MountPath: "/tmp"},
	}
	if err := k8sClient.Create(testCtx, app); err != nil {
		t.Fatal(err)
	}
	if got := app.Spec.Containers[0].ScratchVolumes[0].Medium; got != v1alpha1.ScratchMediumDisk {
		t.Fatalf("want medium defaulted to Disk, got %q", got)
	}
}

func TestSchema_RejectsUnknownPolicyResourceKey(t *testing.T) {
	p := &v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-resource-key"},
		Spec: v1alpha1.WebAppPolicySpec{
			Enforcement: v1alpha1.EnforcementEnforce,
			Resources: &v1alpha1.ResourcePolicy{
				MinRequests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
			},
		},
	}
	err := k8sClient.Create(testCtx, p)
	if err == nil {
		t.Fatal("want rejection for a resource key the policy cannot evaluate")
	}
	if !strings.Contains(err.Error(), "only cpu, memory and ephemeral-storage") {
		t.Fatalf("unexpected error: %v", err)
	}
}
