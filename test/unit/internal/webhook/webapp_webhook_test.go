package webhook_test

import . "github.com/blue-samarth/Warp-CRD/internal/webhook/v1alpha1"

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func baseApp() *v1alpha1.WebApp {
	return &v1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
		Spec: v1alpha1.WebAppSpec{
			Containers: []v1alpha1.Container{{
				Name:  "web",
				Image: "nginx:1.27",
				Ports: []v1alpha1.ContainerPort{{Name: "http", ContainerPort: 8080}},
			}},
		},
	}
}

func autoscaledApp(maxReplicas int32) *v1alpha1.WebApp {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: new(maxReplicas)}
	app.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("100m"),
	}
	return app
}

func fieldErrors(t *testing.T, app *v1alpha1.WebApp) string {
	t.Helper()
	var sb strings.Builder
	for _, e := range ValidateSpec(app) {
		sb.WriteString(e.Error())
		sb.WriteString("\n")
	}
	return sb.String()
}

func TestDefault_SetsReplicasAndServiceType(t *testing.T) {
	app := baseApp()
	Default(app)
	// spec.replicas is defaulted by the schema, not here; defaulting it twice
	// is what made the nil branch unreachable and the warning always fire.
	if app.Spec.Replicas != nil {
		t.Fatalf("the webhook must not default replicas, got %d", *app.Spec.Replicas)
	}
	if app.Spec.ServiceType != corev1.ServiceTypeClusterIP {
		t.Fatalf("want ClusterIP, got %s", app.Spec.ServiceType)
	}
	if app.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatalf("want RollingUpdate, got %s", app.Spec.Strategy.Type)
	}
	if app.Spec.Containers[0].Ports[0].Protocol != corev1.ProtocolTCP {
		t.Fatal("want protocol defaulted to TCP")
	}
}

func TestDefault_DoesNotSetReplicasWhenAutoscaling(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: new(int32(4))}
	Default(app)
	if app.Spec.Replicas != nil {
		t.Fatalf("replicas must stay unset under autoscaling, got %d", *app.Spec.Replicas)
	}
	if *app.Spec.Autoscaling.TargetCPUUtilizationPercentage != 70 {
		t.Fatal("want target CPU defaulted to 70")
	}
	if *app.Spec.Autoscaling.MinReplicas != 1 {
		t.Fatal("want minReplicas defaulted to 1")
	}
}

func TestDefault_LeavesWebAppLabelsAlone(t *testing.T) {
	app := baseApp()
	app.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	Default(app)
	if got := app.Labels["app.kubernetes.io/managed-by"]; got != "Helm" {
		t.Fatalf("defaulting must not claim the user's object, got %q", got)
	}
}

func TestDefault_IsIdempotent(t *testing.T) {
	app := baseApp()
	Default(app)
	first := app.Spec.ServiceType
	Default(app)
	if app.Spec.ServiceType != first {
		t.Fatal("defaulting must be idempotent")
	}
}

func TestValidate_AcceptsValidSpec(t *testing.T) {
	if errs := ValidateSpec(baseApp()); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidate_RequiresContainer(t *testing.T) {
	app := baseApp()
	app.Spec.Containers = nil
	if got := fieldErrors(t, app); !strings.Contains(got, "spec.containers") {
		t.Fatalf("want containers error, got %q", got)
	}
}

func TestValidate_RequiresImageAndName(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Image = ""
	app.Spec.Containers[0].Name = ""
	got := fieldErrors(t, app)
	if !strings.Contains(got, "image") || !strings.Contains(got, "name") {
		t.Fatalf("want image+name errors, got %q", got)
	}
}

func TestValidate_RejectsDuplicatePortNamesAcrossContainers(t *testing.T) {
	app := baseApp()
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:  "sidecar",
		Image: "envoy:v1.30",
		Ports: []v1alpha1.ContainerPort{{Name: "http", ContainerPort: 9901}},
	})
	got := fieldErrors(t, app)
	if !strings.Contains(got, "Duplicate value") || !strings.Contains(got, "web") {
		t.Fatalf("want duplicate port name error naming the other container, got %q", got)
	}
}

func TestValidate_RejectsSamePortInTwoContainers(t *testing.T) {
	app := baseApp()
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:  "sidecar",
		Image: "envoy:v1.30",
		Ports: []v1alpha1.ContainerPort{{Name: "admin", ContainerPort: 8080}},
	})
	// One network namespace: the second bind fails at runtime, and the
	// Service would silently dedupe the pair.
	if got := fieldErrors(t, app); !strings.Contains(got, "share one network namespace") {
		t.Fatalf("want the duplicate port rejected, got %q", got)
	}
}

func TestValidate_AllowsSamePortOnDifferentProtocols(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports = append(app.Spec.Containers[0].Ports,
		v1alpha1.ContainerPort{Name: "dns", ContainerPort: 8080, Protocol: corev1.ProtocolUDP})
	if errs := ValidateSpec(app); len(errs) != 0 {
		t.Fatalf("TCP and UDP on one port is legal, got %v", errs)
	}
}

func TestValidate_RejectsOutOfRangePort(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports[0].ContainerPort = 70000
	if got := fieldErrors(t, app); !strings.Contains(got, "between 1 and 65535") {
		t.Fatalf("want port range error, got %q", got)
	}
}

func TestValidate_RequiresMaxReplicasWhenAutoscaling(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true}
	got := fieldErrors(t, app)
	if !strings.Contains(got, "maxReplicas") || !strings.Contains(got, "Required value") {
		t.Fatalf("want maxReplicas required error, got %q", got)
	}
}

func TestValidate_RejectsMaxBelowMin(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:     true,
		MinReplicas: new(int32(5)),
		MaxReplicas: new(int32(2)),
	}
	if got := fieldErrors(t, app); !strings.Contains(got, "greater than or equal to minReplicas (5)") {
		t.Fatalf("want min/max error, got %q", got)
	}
}

func TestValidate_RejectsBadDomain(t *testing.T) {
	app := baseApp()
	app.Spec.Domain = "Not_A_Domain"
	if got := fieldErrors(t, app); !strings.Contains(got, "spec.domain") {
		t.Fatalf("want domain error, got %q", got)
	}
}

func TestValidate_RejectsRollingUpdateWithRecreate(t *testing.T) {
	app := baseApp()
	app.Spec.Strategy = &v1alpha1.StrategySpec{
		Type:          appsv1.RecreateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{},
	}
	if got := fieldErrors(t, app); !strings.Contains(got, "strategy.rollingUpdate") {
		t.Fatalf("want strategy error, got %q", got)
	}
}

func TestValidate_RequiresAtLeastOnePort(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports = nil
	if got := fieldErrors(t, app); !strings.Contains(got, "at least one container port") {
		t.Fatalf("want port-required error, got %q", got)
	}
}

func TestValidate_PortOnAnyContainerSatisfiesTheRule(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Ports = nil
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:  "sidecar",
		Image: "envoy:v1.30",
		Ports: []v1alpha1.ContainerPort{{Name: "admin", ContainerPort: 9901}},
	})
	if errs := ValidateSpec(app); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidateDelete_AlwaysAllows(t *testing.T) {
	w, err := (WebAppValidator{}).ValidateDelete(t.Context(), baseApp())
	if err != nil || w != nil {
		t.Fatalf("delete must always be allowed, got warnings=%v err=%v", w, err)
	}
}

func TestWarnings_FlagsMutableImageTag(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Image = "nginx:latest"
	w, err := (WebAppValidator{}).ValidateCreate(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) == 0 || !strings.Contains(strings.Join(w, " "), "mutable image tag") {
		t.Fatalf("want a mutable-tag warning, got %v", w)
	}
}

func validatorWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) WebAppValidator {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return WebAppValidator{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
			WithInterceptorFuncs(funcs).Build(),
	}
}

func TestValidateUpdate_SkipsWhenSpecUnchanged(t *testing.T) {
	app := baseApp()
	app.Spec.Replicas = new(int32(99))

	v := validatorWith(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Fatal("an unchanged spec must not re-run policy evaluation")
			return nil
		},
	})
	if _, err := v.ValidateUpdate(t.Context(), app.DeepCopy(), app); err != nil {
		t.Fatalf("want admitted, got %v", err)
	}
}

func TestValidateUpdate_SkipsWhileDeleting(t *testing.T) {
	app := baseApp()
	app.Spec.Containers = nil
	now := metav1.Now()
	app.DeletionTimestamp = &now

	old := app.DeepCopy()
	old.Spec.Containers = baseApp().Spec.Containers

	v := validatorWith(t, interceptor.Funcs{})
	if _, err := v.ValidateUpdate(t.Context(), old, app); err != nil {
		t.Fatalf("a finalizer update during deletion must never be blocked, got %v", err)
	}
}

func TestValidator_NoPoliciesSkipsNamespaceLookup(t *testing.T) {
	v := validatorWith(t, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("namespace must not be fetched when no policies exist")
			return nil
		},
	})
	if _, err := v.ValidateCreate(t.Context(), baseApp()); err != nil {
		t.Fatalf("want admitted, got %v", err)
	}
}

func TestValidator_SurfacesPolicyListErrors(t *testing.T) {
	v := validatorWith(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	})
	_, err := v.ValidateCreate(t.Context(), baseApp())
	if err == nil || !strings.Contains(err.Error(), "list webapppolicies") {
		t.Fatalf("want the list error surfaced, got %v", err)
	}
}

func TestValidator_SurfacesMissingNamespace(t *testing.T) {
	v := validatorWith(t, interceptor.Funcs{}, &v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       v1alpha1.WebAppPolicySpec{MaxReplicas: new(int32(1))},
	})
	_, err := v.ValidateCreate(t.Context(), baseApp())
	if err == nil || !strings.Contains(err.Error(), "get namespace") {
		t.Fatalf("want the namespace lookup error surfaced, got %v", err)
	}
}

func TestValidator_RejectsPolicyViolation(t *testing.T) {
	app := baseApp()
	app.Spec.Replicas = new(int32(9))

	v := validatorWith(t, interceptor.Funcs{},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&v1alpha1.WebAppPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "cap"},
			Spec: v1alpha1.WebAppPolicySpec{
				Enforcement: v1alpha1.EnforcementEnforce,
				MaxReplicas: new(int32(2)),
			},
		})

	_, err := v.ValidateCreate(t.Context(), app)
	if err == nil || !strings.Contains(err.Error(), "at most 2 replicas") {
		t.Fatalf("want policy violation rejected, got %v", err)
	}
}

func TestValidator_WarnModePolicyAdmitsWithWarning(t *testing.T) {
	app := baseApp()
	app.Spec.Replicas = new(int32(9))

	v := validatorWith(t, interceptor.Funcs{},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&v1alpha1.WebAppPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "soft-cap"},
			Spec: v1alpha1.WebAppPolicySpec{
				Enforcement: v1alpha1.EnforcementWarn,
				MaxReplicas: new(int32(2)),
			},
		})

	old := app.DeepCopy()
	old.Spec.Replicas = new(int32(1))

	w, err := v.ValidateUpdate(t.Context(), old, app)
	if err != nil {
		t.Fatalf("warn mode must admit, got %v", err)
	}
	if !strings.Contains(strings.Join(w, " "), "soft-cap") {
		t.Fatalf("want a warning naming the policy, got %v", w)
	}
}

func TestValidate_RejectsNonDNS1123ContainerName(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Name = "Web_Server"
	if got := fieldErrors(t, app); !strings.Contains(got, "containers[0].name") {
		t.Fatalf("want DNS-1123 label error, got %q", got)
	}
}

func TestValidate_RejectsPortNamesKubernetesWouldReject(t *testing.T) {
	for _, name := range []string{"8080", "a--b", "-http", "http-"} {
		app := baseApp()
		app.Spec.Containers[0].Ports[0].Name = name
		if got := fieldErrors(t, app); !strings.Contains(got, "ports[0].name") {
			t.Fatalf("port name %q must be rejected, got %q", name, got)
		}
	}
}

func TestValidate_AcceptsGeneratedStylePortNames(t *testing.T) {
	for _, name := range []string{"http", "port-8080", "h2c", "grpc-web"} {
		app := baseApp()
		app.Spec.Containers[0].Ports[0].Name = name
		if errs := ValidateSpec(app); len(errs) != 0 {
			t.Fatalf("port name %q must be accepted, got %v", name, errs)
		}
	}
}

func TestValidate_RequiresCPURequestWhenAutoscalingOnCPU(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MaxReplicas:                    new(int32(5)),
		TargetCPUUtilizationPercentage: new(int32(70)),
	}
	if got := fieldErrors(t, app); !strings.Contains(got, "resources.requests[cpu]") {
		t.Fatalf("want a cpu-request requirement, got %q", got)
	}
}

func TestValidate_RequiresMemoryRequestWhenAutoscalingOnMemory(t *testing.T) {
	app := autoscaledApp(5)
	app.Spec.Autoscaling.TargetMemoryUtilizationPercentage = new(int32(80))
	if got := fieldErrors(t, app); !strings.Contains(got, "resources.requests[memory]") {
		t.Fatalf("want a memory-request requirement, got %q", got)
	}
}

func TestValidate_FlagsEverySidecarMissingTheRequest(t *testing.T) {
	app := autoscaledApp(5)
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:  "sidecar",
		Image: "envoy:v1.30",
		Ports: []v1alpha1.ContainerPort{{Name: "admin", ContainerPort: 9901}},
	})
	if got := fieldErrors(t, app); !strings.Contains(got, "containers[1].resources.requests[cpu]") {
		t.Fatalf("want the sidecar flagged, got %q", got)
	}
}

func TestValidate_AcceptsNameShapesThatUsedToCollide(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].Name = "web"
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
		{Name: "tmp-cache", MountPath: "/a"},
	}
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:           "web-tmp",
		Image:          "envoy:v1.30",
		Ports:          []v1alpha1.ContainerPort{{Name: "admin", ContainerPort: 9901}},
		ScratchVolumes: []v1alpha1.ScratchVolume{{Name: "cache", MountPath: "/b"}},
	})
	if errs := ValidateSpec(app); len(errs) != 0 {
		t.Fatalf("hashed volume names make this legal, got %v", errs)
	}
}

func TestValidate_RejectsNonPositiveScratchSizeLimit(t *testing.T) {
	for _, size := range []string{"-1Gi", "0"} {
		app := baseApp()
		q := resource.MustParse(size)
		app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
			{Name: "tmp", MountPath: "/tmp", SizeLimit: &q},
		}
		if got := fieldErrors(t, app); !strings.Contains(got, "greater than zero") {
			t.Fatalf("sizeLimit %q must be rejected, got %q", size, got)
		}
	}
}

func TestValidate_RejectsDuplicateScratchMountPaths(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
		{Name: "one", MountPath: "/tmp"},
		{Name: "two", MountPath: "/tmp"},
	}
	if got := fieldErrors(t, app); !strings.Contains(got, "mountPath") {
		t.Fatalf("want duplicate mountPath rejected, got %q", got)
	}
}

func TestValidate_AcceptsScratchVolume(t *testing.T) {
	app := baseApp()
	app.Spec.Containers[0].ScratchVolumes = []v1alpha1.ScratchVolume{
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "cache", MountPath: "/var/cache"},
	}
	if errs := ValidateSpec(app); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidate_RejectsUnknownIngressPortName(t *testing.T) {
	app := baseApp()
	app.Spec.Domain = "my-app.example.com"
	app.Spec.IngressPortName = "nope"
	if got := fieldErrors(t, app); !strings.Contains(got, "ingressPortName") {
		t.Fatalf("want an ingressPortName error, got %q", got)
	}
}

func TestValidate_AcceptsGeneratedIngressPortName(t *testing.T) {
	app := baseApp()
	app.Spec.Domain = "my-app.example.com"
	app.Spec.Containers[0].Ports[0].Name = ""
	app.Spec.IngressPortName = "port-8080"
	if errs := ValidateSpec(app); len(errs) != 0 {
		t.Fatalf("want the generated name accepted, got %v", errs)
	}
}

func TestDefault_TargetCPUNotAddedWhenMemoryTargetSet(t *testing.T) {
	app := baseApp()
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                           true,
		MaxReplicas:                       new(int32(5)),
		TargetMemoryUtilizationPercentage: new(int32(80)),
	}
	Default(app)
	if app.Spec.Autoscaling.TargetCPUUtilizationPercentage != nil {
		t.Fatal("a memory-only autoscaler must not gain a cpu target, which would require cpu requests")
	}
}

func TestValidate_RejectsDuplicateContainerNames(t *testing.T) {
	app := baseApp()
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:  "web",
		Image: "envoy:v1.30",
		Ports: []v1alpha1.ContainerPort{{Name: "admin", ContainerPort: 9901}},
	})
	if got := fieldErrors(t, app); !strings.Contains(got, "Duplicate value") {
		t.Fatalf("want duplicate container name error, got %q", got)
	}
}
