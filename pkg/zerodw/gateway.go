package zerodw

import (
	"context"
	"fmt"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/go-logr/logr"
	apicorev1 "k8s.io/api/core/v1"
	apimetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kyma-project/lifecycle-manager/internal/pkg/flags"
)

const (
	GatewaySecretName = "istio-gateway-secret"
	kcpRootSecretName = "klm-watcher"
	kcpCACertName     = "klm-watcher-serving"
	istioNamespace    = flags.DefaultIstioNamespace
)

type GatewaySecretHandler struct {
	*secretManager
}

func NewGatewaySecretHandler(kcpClient client.Client) *GatewaySecretHandler {
	return &GatewaySecretHandler{
		secretManager: &secretManager{
			kcpClient: kcpClient,
		},
	}
}

func (gsh *GatewaySecretHandler) ManageGatewaySecret(rootSecret *apicorev1.Secret) error {
	gwSecret, err := gsh.findGatewaySecret()

	if isNotFound(err) {
		return gsh.handleNonExisting(rootSecret)
	}
	if err != nil {
		return err
	}

	return gsh.handleExisting(rootSecret, gwSecret)
}

func (gsh *GatewaySecretHandler) handleNonExisting(rootSecret *apicorev1.Secret) error {
	gwSecret := gsh.newGatewaySecret(rootSecret)
	return gsh.create(context.Background(), gwSecret)
}

func (gsh *GatewaySecretHandler) handleExisting(rootSecret *apicorev1.Secret, gwSecret *apicorev1.Secret) error {
	caCert := certmanagerv1.Certificate{}
	if err := gsh.kcpClient.Get(context.Background(),
		client.ObjectKey{Namespace: istioNamespace, Name: kcpCACertName},
		&caCert); err != nil {
		return fmt.Errorf("failed to get CA certificate: %w", err)
	}

	if gwSecretLastModifiedAtValue, ok := gwSecret.Annotations[LastModifiedAtAnnotation]; ok {
		if gwSecretLastModifiedAt, err := time.Parse(time.RFC3339, gwSecretLastModifiedAtValue); err == nil {
			if caCert.Status.NotBefore != nil && gwSecretLastModifiedAt.After(caCert.Status.NotBefore.Time) {
				return nil
			}
		}
	}

	gwSecret.Data["tls.crt"] = rootSecret.Data["tls.crt"]
	gwSecret.Data["tls.key"] = rootSecret.Data["tls.key"]
	gwSecret.Data["ca.crt"] = rootSecret.Data["ca.crt"]
	return gsh.update(context.Background(), gwSecret)
}

func (gsh *GatewaySecretHandler) findGatewaySecret() (*apicorev1.Secret, error) {
	return gsh.findSecret(context.Background(), client.ObjectKey{
		Name:      GatewaySecretName,
		Namespace: istioNamespace,
	})
}

func (gsh *GatewaySecretHandler) newGatewaySecret(rootSecret *apicorev1.Secret) *apicorev1.Secret {
	gwSecret := &apicorev1.Secret{
		TypeMeta: apimetav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: apicorev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: apimetav1.ObjectMeta{
			Name:      GatewaySecretName,
			Namespace: istioNamespace,
		},
		Data: map[string][]byte{
			"tls.crt": rootSecret.Data["tls.crt"],
			"tls.key": rootSecret.Data["tls.key"],
			"ca.crt":  rootSecret.Data["ca.crt"],
		},
	}
	return gwSecret
}

func WatchChangesOnRootCertificate(clientset *kubernetes.Clientset, gatewaySecretHandler *GatewaySecretHandler,
	setupLog logr.Logger,
) {
	secretWatch, err := clientset.CoreV1().Secrets("istio-system").Watch(context.Background(), apimetav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(apimetav1.ObjectNameField, kcpRootSecretName).String(),
	})
	if err != nil {
		setupLog.Error(err, "unable to start watching root certificate")
		return
	}

	for event := range secretWatch.ResultChan() {
		item, ok := event.Object.(*apicorev1.Secret)
		if !ok {
			setupLog.Info("unable to convert object to secret", "object", event.Object)
		}

		switch event.Type {
		case watch.Added:
			fallthrough
		case watch.Modified:
			err := gatewaySecretHandler.ManageGatewaySecret(item)
			if err != nil {
				setupLog.Error(err, "unable to manage istio gateway secret")
			}
		case watch.Deleted:
			fallthrough
		case watch.Error:
			fallthrough
		case watch.Bookmark:
			fallthrough
		default:
			setupLog.Info("ignored event type", "event", event.Type)
		}
	}
}