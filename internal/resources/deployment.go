package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func DesiredReplicas(app *v1alpha1.WebApp) *int32 {
	if app.Spec.Autoscaling != nil && app.Spec.Autoscaling.Enabled {
		return nil
	}
	return app.Spec.Replicas
}

func Deployment(app *v1alpha1.WebApp) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            app.Name,
			Namespace:       app.Namespace,
			Labels:          Labels(app),
			OwnerReferences: OwnerRefs(app),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: DesiredReplicas(app),
			Selector: &metav1.LabelSelector{MatchLabels: SelectorLabels(app)},
			Strategy: strategy(app),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels(app),
					Annotations: app.Spec.PodAnnotations,
				},
				Spec: corev1.PodSpec{
					Containers:                   containers(app),
					ImagePullSecrets:             app.Spec.ImagePullSecrets,
					NodeSelector:                 app.Spec.NodeSelector,
					Tolerations:                  app.Spec.Tolerations,
					Affinity:                     app.Spec.Affinity,
					ServiceAccountName:           app.Spec.ServiceAccountName,
					AutomountServiceAccountToken: automountToken(app),
					SecurityContext:              podSecurityContext(app),
					Volumes:                      scratchVolumes(app),
				},
			},
		},
	}
}

func strategy(app *v1alpha1.WebApp) appsv1.DeploymentStrategy {
	if app.Spec.Strategy == nil || app.Spec.Strategy.Type == "" {
		return appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType}
	}
	s := appsv1.DeploymentStrategy{Type: app.Spec.Strategy.Type}
	if s.Type == appsv1.RollingUpdateDeploymentStrategyType {
		s.RollingUpdate = app.Spec.Strategy.RollingUpdate
	}
	return s
}

func podLabels(app *v1alpha1.WebApp) map[string]string {
	l := map[string]string{}
	maps.Copy(l, app.Spec.PodLabels)
	maps.Copy(l, Labels(app))
	return l
}

func automountToken(app *v1alpha1.WebApp) *bool {
	if app.Spec.AutomountServiceAccountToken != nil {
		return app.Spec.AutomountServiceAccountToken
	}
	return new(false)
}

func podSecurityContext(app *v1alpha1.WebApp) *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{
		RunAsNonRoot:   new(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if s := app.Spec.SecurityContext; s != nil {
		sc.RunAsUser = s.RunAsUser
		sc.RunAsGroup = s.RunAsGroup
		sc.FSGroup = s.FSGroup
	}
	return sc
}

func containerSecurityContext(c v1alpha1.Container) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Privileged:               new(false),
		RunAsNonRoot:             new(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	if s := c.SecurityContext; s != nil {
		sc.ReadOnlyRootFilesystem = s.ReadOnlyRootFilesystem
		sc.RunAsUser = s.RunAsUser
		sc.RunAsGroup = s.RunAsGroup
	}
	return sc
}

func ScratchVolumeName(c v1alpha1.Container, v v1alpha1.ScratchVolume) string {
	sum := sha256.Sum256([]byte(c.Name + "\x00" + v.Name))
	suffix := hex.EncodeToString(sum[:4])
	base := strings.TrimRight(truncate(v.Name, 63-1-len(suffix)), "-")
	return base + "-" + suffix
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func scratchVolumes(app *v1alpha1.WebApp) []corev1.Volume {
	var out []corev1.Volume
	for _, c := range app.Spec.Containers {
		for _, v := range c.ScratchVolumes {
			src := &corev1.EmptyDirVolumeSource{SizeLimit: v.SizeLimit}
			if v.Medium == v1alpha1.ScratchMediumMemory {
				src.Medium = corev1.StorageMediumMemory
			}
			out = append(out, corev1.Volume{
				Name:         ScratchVolumeName(c, v),
				VolumeSource: corev1.VolumeSource{EmptyDir: src},
			})
		}
	}
	return out
}

func volumeMounts(c v1alpha1.Container) []corev1.VolumeMount {
	if len(c.ScratchVolumes) == 0 {
		return nil
	}
	out := make([]corev1.VolumeMount, 0, len(c.ScratchVolumes))
	for _, v := range c.ScratchVolumes {
		out = append(out, corev1.VolumeMount{
			Name:      ScratchVolumeName(c, v),
			MountPath: v.MountPath,
		})
	}
	return out
}

func containers(app *v1alpha1.WebApp) []corev1.Container {
	out := make([]corev1.Container, 0, len(app.Spec.Containers))
	for _, c := range app.Spec.Containers {
		out = append(out, corev1.Container{
			Name:            c.Name,
			Image:           c.Image,
			Command:         c.Command,
			Args:            c.Args,
			Ports:           containerPorts(c.Ports),
			Env:             c.Env,
			EnvFrom:         c.EnvFrom,
			Resources:       c.Resources,
			LivenessProbe:   c.LivenessProbe,
			ReadinessProbe:  c.ReadinessProbe,
			StartupProbe:    c.StartupProbe,
			SecurityContext: containerSecurityContext(c),
			VolumeMounts:    volumeMounts(c),
		})
	}
	return out
}

func containerPorts(ports []v1alpha1.ContainerPort) []corev1.ContainerPort {
	if len(ports) == 0 {
		return nil
	}
	out := make([]corev1.ContainerPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, corev1.ContainerPort{
			Name:          portName(p),
			ContainerPort: p.ContainerPort,
			Protocol:      protocol(p),
		})
	}
	return out
}
