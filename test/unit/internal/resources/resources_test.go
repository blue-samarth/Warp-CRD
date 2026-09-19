package resources_test

import . "github.com/blue-samarth/Warp-CRD/internal/resources"

import (
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func baseApp() *v1alpha1.WebApp {
	return &v1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default", UID: "uid-1"},
		Spec: v1alpha1.WebAppSpec{
			Containers: []v1alpha1.Container{{
				Name:  "web",
				Image: "nginx:1.27",
				Ports: []v1alpha1.ContainerPort{{Name: "http", ContainerPort: 8080}},
			}},
		},
	}
}

func TestDesiredReplicas_NilWhenUnset(t *testing.T) {
	// Unset means "not ours": sending a value would snap a scaled app back.
	if got := DesiredReplicas(baseApp()); got != nil {
		t.Fatalf("want nil so the field stays unmanaged, got %d", *got)
	}
}

func TestDesiredReplicas_PassesThroughWhenSet(t *testing.T) {
	app := baseApp()
	app.Spec.Replicas = new(int32(4))
	if got := DesiredReplicas(app); got == nil || *got != 4 {
		t.Fatalf("want 4, got %v", got)
	}
}

func TestDesiredReplicas_NilWhenAutoscalingEnabled(t *testing.T) {
	app := baseApp()
	app.Spec.Replicas = new(int32(5))
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: new(int32(9))}
	if got := DesiredReplicas(app); got != nil {
		t.Fatalf("want nil so the HPA owns replicas, got %d", *got)
	}
}

func TestDeployment_SelectorIncludesManagedBy(t *testing.T) {
	dep := Deployment(baseApp())
	if dep.Spec.Selector.MatchLabels[ManagedByLabel] != ManagedByValue {
		t.Fatal("selector must include managed-by so it cannot overlap another tool's workload")
	}
	if dep.Spec.Template.Labels[ManagedByLabel] != ManagedByValue {
		t.Fatal("pod template must match the selector")
	}
}

func TestDeployment_OwnerReferenceIsController(t *testing.T) {
	dep := Deployment(baseApp())
	refs := dep.OwnerReferences
	if len(refs) != 1 {
		t.Fatalf("want 1 owner ref, got %d", len(refs))
	}
	if refs[0].Controller == nil || !*refs[0].Controller {
		t.Fatal("owner ref must be a controller ref")
	}
	if refs[0].Kind != "WebApp" || refs[0].UID != "uid-1" {
		t.Fatalf("unexpected owner ref: %+v", refs[0])
	}
}

func TestDeployment_RecreateDropsRollingUpdate(t *testing.T) {
	app := baseApp()
	app.Spec.Strategy = &v1alpha1.StrategySpec{
		Type:          appsv1.RecreateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{},
	}
	dep := Deployment(app)
	if dep.Spec.Strategy.RollingUpdate != nil {
		t.Fatal("rollingUpdate must be dropped for Recreate; the API server rejects the combination")
	}
}

func TestDeployment_MultiContainer(t *testing.T) {
	app := baseApp()
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:  "sidecar",
		Image: "envoy:v1.30",
		Ports: []v1alpha1.ContainerPort{{Name: "admin", ContainerPort: 9901}},
	})
	dep := Deployment(app)
	if len(dep.Spec.Template.Spec.Containers) != 2 {
		t.Fatalf("want 2 containers, got %d", len(dep.Spec.Template.Spec.Containers))
	}
}

func TestService_ExposesAllPortsAcrossContainers(t *testing.T) {
	app := baseApp()
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:  "sidecar",
		Image: "envoy:v1.30",
		Ports: []v1alpha1.ContainerPort{{Name: "admin", ContainerPort: 9901}},
	})
	svc := Service(app)
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("want 2 service ports, got %d", len(svc.Spec.Ports))
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("want ClusterIP default, got %s", svc.Spec.Type)
	}
}

func TestService_DedupesRepeatedPortNumbers(t *testing.T) {
	app := baseApp()
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:  "sidecar",
		Image: "envoy:v1.30",
		Ports: []v1alpha1.ContainerPort{{Name: "dup", ContainerPort: 8080}},
	})
	if got := len(Service(app).Spec.Ports); got != 1 {
		t.Fatalf("want ports deduped to 1, got %d", got)
	}
}

func TestService_UnnamedPortGetsGeneratedName(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports = []v1alpha1.ContainerPort{{ContainerPort: 3000}}
	if got := Service(app).Spec.Ports[0].Name; got != "port-3000" {
		t.Fatalf("want port-3000, got %q", got)
	}
}

func TestIngress_PrefersPortNamedHTTP(t *testing.T) {
	app := baseApp()
	app.Spec.Domain = "my-app.example.com"
	app.Spec.Containers[0].Ports = []v1alpha1.ContainerPort{
		{Name: "metrics", ContainerPort: 9090},
		{Name: "http", ContainerPort: 8080},
	}
	ing, err := Ingress(app)
	if err != nil {
		t.Fatal(err)
	}
	got := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name
	if got != "http" {
		t.Fatalf("want backend port http, got %q", got)
	}
}

func TestIngress_FallsBackToFirstPort(t *testing.T) {
	app := baseApp()
	app.Spec.Domain = "my-app.example.com"
	app.Spec.Containers[0].Ports = []v1alpha1.ContainerPort{{Name: "metrics", ContainerPort: 9090}}
	ing, err := Ingress(app)
	if err != nil {
		t.Fatal(err)
	}
	if got := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name; got != "metrics" {
		t.Fatalf("want metrics, got %q", got)
	}
}

func TestDeployment_EmitsRestrictedSecurityContexts(t *testing.T) {
	dep := Deployment(baseApp())
	pod := dep.Spec.Template.Spec

	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil ||
		!*pod.SecurityContext.RunAsNonRoot {
		t.Fatal("want runAsNonRoot on the pod")
	}
	if pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("want a RuntimeDefault seccomp profile, got %+v", pod.SecurityContext.SeccompProfile)
	}

	sc := pod.Containers[0].SecurityContext
	switch {
	case sc == nil:
		t.Fatal("want a container security context")
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Fatal("want allowPrivilegeEscalation=false")
	case sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL":
		t.Fatalf("want all capabilities dropped, got %+v", sc.Capabilities)
	}
}

func TestDeployment_SecurityContextIdentityComesFromSpec(t *testing.T) {
	app := baseApp()
	app.Spec.SecurityContext = &v1alpha1.PodSecurityContext{
		RunAsUser: new(int64(10001)),
		FSGroup:   new(int64(20001)),
	}
	app.Spec.Containers[0].SecurityContext = &v1alpha1.ContainerSecurityContext{
		ReadOnlyRootFilesystem: new(true),
	}
	pod := Deployment(app).Spec.Template.Spec

	if pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != 10001 {
		t.Fatalf("want runAsUser 10001, got %v", pod.SecurityContext.RunAsUser)
	}
	if pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 20001 {
		t.Fatalf("want fsGroup 20001, got %v", pod.SecurityContext.FSGroup)
	}
	sc := pod.Containers[0].SecurityContext
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatal("want readOnlyRootFilesystem honoured")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatal("the hardening half must not be overridable")
	}
}

func TestDeployment_ScratchVolumeIsMountedAndDeclared(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].SecurityContext = &v1alpha1.ContainerSecurityContext{
		ReadOnlyRootFilesystem: new(true),
	}
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
		{Name: "tmp", MountPath: "/tmp"},
	}
	pod := Deployment(app).Spec.Template.Spec

	if len(pod.Volumes) != 1 || pod.Volumes[0].EmptyDir == nil {
		t.Fatalf("want one emptyDir volume, got %+v", pod.Volumes)
	}
	want := ScratchVolumeName(app.Spec.Containers[0], app.Spec.Containers[0].ScratchVolumes[0])
	if pod.Volumes[0].Name != want {
		t.Fatalf("want volume %q, got %q", want, pod.Volumes[0].Name)
	}
	mounts := pod.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].MountPath != "/tmp" || mounts[0].Name != want {
		t.Fatalf("want /tmp mounted from %q, got %+v", want, mounts)
	}
}

func TestDeployment_ScratchVolumeMediumAndSizeLimit(t *testing.T) {
	app := baseApp()
	size := resource.MustParse("64Mi")
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
		{Name: "cache", MountPath: "/cache", Medium: v1alpha1.ScratchMediumMemory, SizeLimit: &size},
	}
	vol := Deployment(app).Spec.Template.Spec.Volumes[0]

	if vol.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("want a tmpfs volume, got medium %q", vol.EmptyDir.Medium)
	}
	if vol.EmptyDir.SizeLimit == nil || vol.EmptyDir.SizeLimit.String() != "64Mi" {
		t.Fatalf("want the size limit carried through, got %v", vol.EmptyDir.SizeLimit)
	}
}

func TestDeployment_ScratchVolumesDoNotCollideAcrossContainers(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{{Name: "tmp", MountPath: "/tmp"}}
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:           "sidecar",
		Image:          "envoy:v1.30",
		Ports:          []v1alpha1.ContainerPort{{Name: "admin", ContainerPort: 9901}},
		ScratchVolumes: []v1alpha1.ScratchVolume{{Name: "tmp", MountPath: "/tmp"}},
	})
	pod := Deployment(app).Spec.Template.Spec

	if len(pod.Volumes) != 2 {
		t.Fatalf("want a volume per container, got %+v", pod.Volumes)
	}
	if pod.Volumes[0].Name == pod.Volumes[1].Name {
		t.Fatalf("same-named scratch volumes must not collide, both are %q", pod.Volumes[0].Name)
	}
	want := ScratchVolumeName(app.Spec.Containers[1], app.Spec.Containers[1].ScratchVolumes[0])
	if len(pod.Containers[1].VolumeMounts) != 1 || pod.Containers[1].VolumeMounts[0].Name != want {
		t.Fatalf("sidecar mounted the wrong volume: %+v", pod.Containers[1].VolumeMounts)
	}
}

func TestScratchVolumeName_DisambiguatesAmbiguousPairs(t *testing.T) {
	// "web"+"tmp-cache" and "web-tmp"+"cache" concatenate identically.
	a := ScratchVolumeName(
		v1alpha1.Container{Name: "web"},
		v1alpha1.ScratchVolume{Name: "tmp-cache"})
	b := ScratchVolumeName(
		v1alpha1.Container{Name: "web-tmp"},
		v1alpha1.ScratchVolume{Name: "cache"})
	if a == b {
		t.Fatalf("names must not collide, both are %q", a)
	}
}

func TestScratchVolumeName_FitsDNSLabel(t *testing.T) {
	long := strings.Repeat("a", 63)
	got := ScratchVolumeName(
		v1alpha1.Container{Name: long},
		v1alpha1.ScratchVolume{Name: long})
	if len(got) > 63 {
		t.Fatalf("want at most 63 characters, got %d (%q)", len(got), got)
	}
	for _, msg := range validation.IsDNS1123Label(got) {
		t.Fatalf("generated name %q is not a DNS-1123 label: %s", got, msg)
	}
}

func TestDeployment_NoVolumesWithoutScratch(t *testing.T) {
	pod := Deployment(baseApp()).Spec.Template.Spec
	if pod.Volumes != nil || pod.Containers[0].VolumeMounts != nil {
		t.Fatalf("want no volumes at all, got %+v / %+v", pod.Volumes, pod.Containers[0].VolumeMounts)
	}
}

func TestDeployment_PassesProbesThrough(t *testing.T) {
	app := baseApp()
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"},
		},
	}
	app.Spec.Containers[0].ReadinessProbe = probe
	app.Spec.Containers[0].LivenessProbe = probe
	app.Spec.Containers[0].StartupProbe = probe

	c := Deployment(app).Spec.Template.Spec.Containers[0]
	if c.ReadinessProbe == nil || c.LivenessProbe == nil || c.StartupProbe == nil {
		t.Fatal("want all three probes on the container")
	}
	if c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("unexpected probe: %+v", c.ReadinessProbe)
	}
}

func TestDeployment_PodMetadataMergesUnderOperatorLabels(t *testing.T) {
	app := baseApp()
	app.Spec.PodLabels = map[string]string{
		"team":                   "payments",
		"app.kubernetes.io/name": "hijacked",
	}
	app.Spec.PodAnnotations = map[string]string{"prometheus.io/scrape": "true"}

	tpl := Deployment(app).Spec.Template
	if tpl.Labels["team"] != "payments" {
		t.Fatalf("want the user label kept, got %v", tpl.Labels)
	}
	if tpl.Labels["app.kubernetes.io/name"] != "my-app" {
		t.Fatal("a podLabel must not be able to break the selector")
	}
	if tpl.Annotations["prometheus.io/scrape"] != "true" {
		t.Fatalf("want pod annotations passed through, got %v", tpl.Annotations)
	}
}

func TestDeployment_SetsServiceAccountName(t *testing.T) {
	app := baseApp()
	app.Spec.ServiceAccountName = "storefront"
	if got := Deployment(app).Spec.Template.Spec.ServiceAccountName; got != "storefront" {
		t.Fatalf("want the service account set, got %q", got)
	}
}

func TestIngress_UsesExplicitPortName(t *testing.T) {
	app := baseApp()
	app.Spec.Domain = "my-app.example.com"
	app.Spec.Containers[0].Ports = append(app.Spec.Containers[0].Ports,
		v1alpha1.ContainerPort{Name: "admin", ContainerPort: 9901})
	app.Spec.IngressPortName = "admin"

	ing, err := Ingress(app)
	if err != nil {
		t.Fatal(err)
	}
	got := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name
	if got != "admin" {
		t.Fatalf("want the explicit port to win over http, got %q", got)
	}
}

func TestIngress_ErrorsOnUnknownExplicitPortName(t *testing.T) {
	app := baseApp()
	app.Spec.Domain = "my-app.example.com"
	app.Spec.IngressPortName = "nope"
	if _, err := Ingress(app); !errors.Is(err, ErrUnknownIngressPort) {
		t.Fatalf("want ErrUnknownIngressPort, got %v", err)
	}
}

func TestPortNames_IncludesGeneratedNames(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports = []v1alpha1.ContainerPort{
		{ContainerPort: 8080},
		{Name: "admin", ContainerPort: 9901},
	}
	got := PortNames(app)
	if len(got) != 2 || got[0] != "port-8080" || got[1] != "admin" {
		t.Fatalf("unexpected port names: %v", got)
	}
}

func TestIngress_ErrorsWithoutPorts(t *testing.T) {
	app := baseApp()
	app.Spec.Domain = "my-app.example.com"
	app.Spec.Containers[0].Ports = nil
	if _, err := Ingress(app); !errors.Is(err, ErrNoPorts) {
		t.Fatalf("want ErrNoPorts, got %v", err)
	}
}

func TestIngressURL_EmptyWithoutDomain(t *testing.T) {
	if got := IngressURL(baseApp()); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestHPA_UsesDefaultsWhenUnset(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: new(int32(10))}
	h, err := HPA(app)
	if err != nil {
		t.Fatal(err)
	}
	if h.Spec.MaxReplicas != 10 {
		t.Fatalf("want max 10, got %d", h.Spec.MaxReplicas)
	}
	if h.Spec.MinReplicas == nil || *h.Spec.MinReplicas != 1 {
		t.Fatalf("want min 1, got %v", h.Spec.MinReplicas)
	}
	target := h.Spec.Metrics[0].Resource.Target.AverageUtilization
	if target == nil || *target != 70 {
		t.Fatalf("want target 70, got %v", target)
	}
	if h.Spec.ScaleTargetRef.Name != "my-app" || h.Spec.ScaleTargetRef.Kind != "Deployment" {
		t.Fatalf("unexpected scale target: %+v", h.Spec.ScaleTargetRef)
	}
}

func TestHPA_ErrorsWithoutMaxReplicas(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true}
	if _, err := HPA(app); !errors.Is(err, ErrNoMaxReplicas) {
		t.Fatalf("want ErrNoMaxReplicas rather than a silent cap of 1, got %v", err)
	}
}

func TestHPA_AddsMemoryMetric(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                           true,
		MaxReplicas:                       new(int32(10)),
		TargetCPUUtilizationPercentage:    new(int32(60)),
		TargetMemoryUtilizationPercentage: new(int32(80)),
	}
	h, err := HPA(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Spec.Metrics) != 2 {
		t.Fatalf("want cpu and memory metrics, got %+v", h.Spec.Metrics)
	}
	got := map[corev1.ResourceName]int32{}
	for _, m := range h.Spec.Metrics {
		got[m.Resource.Name] = *m.Resource.Target.AverageUtilization
	}
	if got[corev1.ResourceCPU] != 60 || got[corev1.ResourceMemory] != 80 {
		t.Fatalf("unexpected targets: %v", got)
	}
}

func TestHPA_OmitsCPUWhenOnlyMemoryTargeted(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                           true,
		MaxReplicas:                       new(int32(10)),
		TargetMemoryUtilizationPercentage: new(int32(80)),
	}
	h, err := HPA(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Spec.Metrics) != 1 || h.Spec.Metrics[0].Resource.Name != corev1.ResourceMemory {
		t.Fatalf("want memory only, got %+v", h.Spec.Metrics)
	}
}

func TestHPA_PassesBehaviorThrough(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:     true,
		MaxReplicas: new(int32(10)),
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: new(int32(600))},
		},
	}
	h, err := HPA(app)
	if err != nil {
		t.Fatal(err)
	}
	if h.Spec.Behavior == nil || *h.Spec.Behavior.ScaleDown.StabilizationWindowSeconds != 600 {
		t.Fatalf("want the behavior preserved, got %+v", h.Spec.Behavior)
	}
}

func TestWantsHPA_FalseWhenDisabled(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: false, MaxReplicas: new(int32(10))}
	if WantsHPA(app) {
		t.Fatal("want false when autoscaling disabled")
	}
}

func TestDeployment_ContainerWithoutPorts(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports = nil
	dep := Deployment(app)
	if got := dep.Spec.Template.Spec.Containers[0].Ports; got != nil {
		t.Fatalf("want nil ports, got %v", got)
	}
}

func TestService_NoPortsProducesEmptyList(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports = nil
	if got := len(Service(app).Spec.Ports); got != 0 {
		t.Fatalf("want 0 ports, got %d", got)
	}
}

func TestDeployment_PropagatesSchedulingFields(t *testing.T) {
	app := baseApp()
	app.Spec.NodeSelector = map[string]string{"disk": "ssd"}
	app.Spec.Tolerations = []corev1.Toleration{{Key: "spot", Operator: corev1.TolerationOpExists}}
	app.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "regcred"}}

	spec := Deployment(app).Spec.Template.Spec
	if spec.NodeSelector["disk"] != "ssd" {
		t.Fatal("nodeSelector not propagated")
	}
	if len(spec.Tolerations) != 1 || len(spec.ImagePullSecrets) != 1 {
		t.Fatalf("scheduling fields not propagated: %+v", spec)
	}
}

func TestIngress_UsesIngressClassName(t *testing.T) {
	app := baseApp()
	app.Spec.Domain = "app.example.com"
	app.Spec.IngressClassName = new("nginx")
	ing, err := Ingress(app)
	if err != nil {
		t.Fatal(err)
	}
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" {
		t.Fatalf("ingressClassName not propagated: %v", ing.Spec.IngressClassName)
	}
}

func TestService_SamePortOnTCPAndUDPBothSurvive(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports = []v1alpha1.ContainerPort{
		{Name: "dns-tcp", ContainerPort: 53, Protocol: corev1.ProtocolTCP},
		{Name: "dns-udp", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
	}
	ports := Service(app).Spec.Ports
	if len(ports) != 2 {
		t.Fatalf("a port number may be exposed on both protocols; got %d ports: %+v", len(ports), ports)
	}
	protos := map[corev1.Protocol]bool{}
	for _, p := range ports {
		protos[p.Protocol] = true
	}
	if !protos[corev1.ProtocolTCP] || !protos[corev1.ProtocolUDP] {
		t.Fatalf("want both protocols preserved, got %+v", ports)
	}
}

func TestService_UnnamedPortsDisambiguateByProtocol(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports = []v1alpha1.ContainerPort{
		{ContainerPort: 53, Protocol: corev1.ProtocolTCP},
		{ContainerPort: 53, Protocol: corev1.ProtocolUDP},
	}
	names := map[string]bool{}
	for _, p := range Service(app).Spec.Ports {
		if names[p.Name] {
			t.Fatalf("duplicate service port name %q; Service port names must be unique", p.Name)
		}
		names[p.Name] = true
	}
	if !names["port-53"] || !names["port-53-udp"] {
		t.Fatalf("unexpected generated names: %v", names)
	}
}
