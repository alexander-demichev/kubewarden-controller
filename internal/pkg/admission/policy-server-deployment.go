package admission

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"

	v1alpha2 "github.com/kubewarden/kubewarden-controller/apis/v1alpha2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubewarden/kubewarden-controller/internal/pkg/constants"
)

// reconcilePolicyServerDeployment reconciles the Deployment that runs the PolicyServer
// component
func (r *Reconciler) reconcilePolicyServerDeployment(ctx context.Context, policyServer *v1alpha2.PolicyServer) error {
	configMapVersion, err := r.policyServerConfigMapVersion(ctx, policyServer)
	if err != nil {
		return fmt.Errorf("cannot get policy-server ConfigMap version: %w", err)
	}

	if policyServer.Spec.ImagePullSecret != "" {
		err := r.policyServerImagePullSecretPresent(ctx, policyServer)
		if err != nil {
			return err
		}
	}

	deployment := r.deployment(configMapVersion, policyServer)
	err = r.Client.Create(ctx, deployment)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("error reconciling policy-server deployment: %w", err)
	}

	return r.updatePolicyServerDeployment(ctx, policyServer, deployment)
}

func (r *Reconciler) policyServerImagePullSecretPresent(ctx context.Context, policyServer *v1alpha2.PolicyServer) error {
	// By using Unstructured data we force the client to fetch fresh, uncached
	// data from the API server
	unstructuredObj := &unstructured.Unstructured{}
	unstructuredObj.SetGroupVersionKind(schema.GroupVersionKind{
		Kind:    "Secret",
		Version: "v1",
	})
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: r.DeploymentsNamespace,
		Name:      policyServer.Spec.ImagePullSecret,
	}, unstructuredObj)
	if err != nil {
		return fmt.Errorf("cannot get spec.ImagePullSecret: %w", err)
	}

	var secret corev1.Secret
	err = runtime.DefaultUnstructuredConverter.
		FromUnstructured(unstructuredObj.UnstructuredContent(), &secret)
	if err != nil {
		return fmt.Errorf("spec.ImagePullSecret is not of Kind Secret: %w", err)
	}

	if secret.Type != "kubernetes.io/dockerconfigjson" {
		return fmt.Errorf("spec.ImagePullSecret secret \"%s\" is not of type kubernetes.io/dockerconfigjson", secret.Name)
	}
	return nil
}

func (r *Reconciler) updatePolicyServerDeployment(ctx context.Context, policyServer *v1alpha2.PolicyServer, newDeployment *appsv1.Deployment) error {
	originalDeployment := &appsv1.Deployment{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: r.DeploymentsNamespace,
		Name:      policyServer.NameWithPrefix(),
	}, originalDeployment)
	if err != nil {
		return fmt.Errorf("cannot retrieve existing policy-server Deployment: %w", err)
	}

	if shouldUpdatePolicyServerDeployment(originalDeployment, newDeployment) {
		patch := originalDeployment.DeepCopy()
		patch.Spec.Replicas = newDeployment.Spec.Replicas
		patch.Spec.Template = newDeployment.Spec.Template
		patch.ObjectMeta.Annotations[constants.PolicyServerDeploymentConfigVersionAnnotation] = newDeployment.Annotations[constants.PolicyServerDeploymentConfigVersionAnnotation]
		err = r.Client.Patch(ctx, patch, client.MergeFrom(originalDeployment))
		if err != nil {
			return fmt.Errorf("cannot patch policy-server Deployment: %w", err)
		}
		r.Log.Info("deployment patched")
	}

	return nil
}

func shouldUpdatePolicyServerDeployment(originalDeployment *appsv1.Deployment, newDeployment *appsv1.Deployment) bool {
	return *originalDeployment.Spec.Replicas != *newDeployment.Spec.Replicas ||
		originalDeployment.Spec.Template.Spec.Containers[0].Image != newDeployment.Spec.Template.Spec.Containers[0].Image ||
		originalDeployment.Spec.Template.Spec.ServiceAccountName != newDeployment.Spec.Template.Spec.ServiceAccountName ||
		originalDeployment.Annotations[constants.PolicyServerDeploymentConfigVersionAnnotation] != newDeployment.Annotations[constants.PolicyServerDeploymentConfigVersionAnnotation] ||
		!reflect.DeepEqual(originalDeployment.Spec.Template.Spec.Containers[0].Env, newDeployment.Spec.Template.Spec.Containers[0].Env) ||
		!haveEqualAnnotationsWithoutRestart(originalDeployment, newDeployment)
}

func haveEqualAnnotationsWithoutRestart(originalDeployment *appsv1.Deployment, newDeployment *appsv1.Deployment) bool {
	if originalDeployment.Spec.Template.Annotations == nil && newDeployment.Spec.Template.Annotations == nil {
		return true
	}
	annotationsWithoutRestart := make(map[string]string)
	for k, v := range originalDeployment.Spec.Template.Annotations {
		if k != constants.PolicyServerDeploymentRestartAnnotation {
			annotationsWithoutRestart[k] = v
		}
	}
	return reflect.DeepEqual(annotationsWithoutRestart, newDeployment.Spec.Template.Annotations)
}

func (r *Reconciler) deployment(configMapVersion string, policyServer *v1alpha2.PolicyServer) *appsv1.Deployment {
	const (
		certsVolumeName                  = "certs"
		policiesConfigContainerPath      = "/config"
		policiesFilename                 = "policies.yml"
		sourcesFilename                  = "sources.yml"
		verificationFilename             = "verification.yml"
		policiesVolumeName               = "policies"
		sourcesVolumeName                = "sources"
		verificationConfigVolumeName     = "verification"
		secretsContainerPath             = "/pki"
		imagePullSecretVolumeName        = "imagepullsecret"
		dockerConfigJSONPolicyServerPath = "/home/kubewarden/.docker"
		policyStoreVolume                = "policy-store"
		policyStoreVolumePath            = "/tmp"
		sigstoreCacheDirPath             = "/tmp/sigstore-data"
	)

	admissionContainer := corev1.Container{
		Name:  policyServer.NameWithPrefix(),
		Image: policyServer.Spec.Image,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      certsVolumeName,
				ReadOnly:  true,
				MountPath: secretsContainerPath,
			},
			{
				Name:      policiesVolumeName,
				ReadOnly:  true,
				MountPath: policiesConfigContainerPath,
			},
			{
				Name:      policyStoreVolume,
				MountPath: policyStoreVolumePath,
			},
		},
		Env: append([]corev1.EnvVar{
			{
				Name:  "KUBEWARDEN_CERT_FILE",
				Value: filepath.Join(secretsContainerPath, constants.PolicyServerTLSCert),
			},
			{
				Name:  "KUBEWARDEN_KEY_FILE",
				Value: filepath.Join(secretsContainerPath, constants.PolicyServerTLSKey),
			},
			{
				Name:  "KUBEWARDEN_PORT",
				Value: fmt.Sprintf("%d", constants.PolicyServerPort),
			},
			{
				Name:  "KUBEWARDEN_POLICIES_DOWNLOAD_DIR",
				Value: policyStoreVolumePath,
			},
			{
				Name:  "KUBEWARDEN_POLICIES",
				Value: filepath.Join(policiesConfigContainerPath, policiesFilename),
			},
			{
				Name:  "KUBEWARDEN_SIGSTORE_CACHE_DIR",
				Value: sigstoreCacheDirPath,
			},
		}, policyServer.Spec.Env...),
		ReadinessProbe: &corev1.Probe{
			Handler: corev1.Handler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   constants.PolicyServerReadinessProbe,
					Port:   intstr.FromInt(constants.PolicyServerPort),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
		},
	}
	if policyServer.Spec.VerificationConfig != "" {
		admissionContainer.VolumeMounts = append(admissionContainer.VolumeMounts,
			corev1.VolumeMount{
				Name:      verificationConfigVolumeName,
				ReadOnly:  true,
				MountPath: constants.PolicyServerVerificationConfigContainerPath,
			},
		)
		admissionContainer.Env = append(admissionContainer.Env,
			corev1.EnvVar{
				Name:  "KUBEWARDEN_VERIFICATION_CONFIG_PATH",
				Value: filepath.Join(constants.PolicyServerVerificationConfigContainerPath, verificationFilename),
			},
		)
	}
	if policyServer.Spec.ImagePullSecret != "" {
		admissionContainer.VolumeMounts = append(admissionContainer.VolumeMounts,
			corev1.VolumeMount{
				Name:      imagePullSecretVolumeName,
				ReadOnly:  true,
				MountPath: dockerConfigJSONPolicyServerPath,
			},
		)
		admissionContainer.Env = append(admissionContainer.Env,
			corev1.EnvVar{
				Name:  "KUBEWARDEN_DOCKER_CONFIG_JSON_PATH",
				Value: filepath.Join(dockerConfigJSONPolicyServerPath, ".dockerconfigjson"),
			},
		)
	}
	if len(policyServer.Spec.InsecureSources) > 0 || len(policyServer.Spec.SourceAuthorities) > 0 {
		admissionContainer.VolumeMounts = append(admissionContainer.VolumeMounts,
			corev1.VolumeMount{
				Name:      sourcesVolumeName,
				ReadOnly:  true,
				MountPath: constants.PolicyServerSourcesConfigContainerPath,
			},
		)
		admissionContainer.Env = append(admissionContainer.Env,
			corev1.EnvVar{
				Name:  "KUBEWARDEN_SOURCES_PATH",
				Value: filepath.Join(constants.PolicyServerSourcesConfigContainerPath, sourcesFilename),
			},
		)
	}
	enableReadOnlyFilesystem := true
	admissionContainerSecurityContext := corev1.SecurityContext{
		ReadOnlyRootFilesystem: &enableReadOnlyFilesystem,
	}
	admissionContainer.SecurityContext = &admissionContainerSecurityContext

	templateAnnotations := policyServer.Spec.Annotations
	if templateAnnotations == nil {
		templateAnnotations = make(map[string]string)
	}

	policyServerDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyServer.NameWithPrefix(),
			Namespace: r.DeploymentsNamespace,
			Annotations: map[string]string{
				constants.PolicyServerDeploymentConfigVersionAnnotation: configMapVersion,
			},
			Labels: map[string]string{
				constants.AppLabelKey:          policyServer.AppLabel(),
				constants.PolicyServerLabelKey: policyServer.Name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &policyServer.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					constants.AppLabelKey: policyServer.AppLabel(),
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.AppLabelKey: policyServer.AppLabel(),
						constants.PolicyServerDeploymentPodSpecConfigVersionLabel: configMapVersion,
						constants.PolicyServerLabelKey:                            policyServer.Name,
					},
					Annotations: templateAnnotations,
				},
				Spec: corev1.PodSpec{
					Containers:         []corev1.Container{admissionContainer},
					ServiceAccountName: policyServer.Spec.ServiceAccountName,
					Volumes: []corev1.Volume{
						{
							Name: policyStoreVolume,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: certsVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: policyServer.NameWithPrefix(),
								},
							},
						},
						{
							Name: policiesVolumeName,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: policyServer.NameWithPrefix(),
									},
									Items: []corev1.KeyToPath{
										{
											Key:  constants.PolicyServerConfigPoliciesEntry,
											Path: policiesFilename,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	if policyServer.Spec.VerificationConfig != "" {
		policyServerDeployment.Spec.Template.Spec.Volumes = append(
			policyServerDeployment.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: verificationConfigVolumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: policyServer.Spec.VerificationConfig,
						},
						Items: []corev1.KeyToPath{
							{
								Key:  constants.PolicyServerVerificationConfigEntry,
								Path: verificationFilename,
							},
						},
					},
				},
			},
		)
	}
	if policyServer.Spec.ImagePullSecret != "" {
		policyServerDeployment.Spec.Template.Spec.Volumes = append(
			policyServerDeployment.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: imagePullSecretVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: policyServer.Spec.ImagePullSecret,
					},
				},
			},
		)
	}
	if len(policyServer.Spec.InsecureSources) > 0 || len(policyServer.Spec.SourceAuthorities) > 0 {
		policyServerDeployment.Spec.Template.Spec.Volumes = append(
			policyServerDeployment.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: sourcesVolumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: policyServer.NameWithPrefix(),
						},
					},
				},
			},
		)
	}

	return policyServerDeployment
}
