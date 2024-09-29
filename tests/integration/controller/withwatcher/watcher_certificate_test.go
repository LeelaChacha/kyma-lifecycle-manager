package withwatcher_test

import (
	"bytes"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	apicorev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kyma-project/lifecycle-manager/api/shared"
	"github.com/kyma-project/lifecycle-manager/internal/pkg/flags"
	"github.com/kyma-project/lifecycle-manager/pkg/util"
	"github.com/kyma-project/lifecycle-manager/pkg/watcher"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/kyma-project/lifecycle-manager/pkg/testutils"
)

var ErrPrivateKeyNotMatching = errors.New("private Key for the TLS secret doesn't match")

var _ = Describe("Watcher Certificate Configuration in remote sync mode", Ordered, func() {
	kyma := NewTestKyma("kyma-remote-sync-cert")

	watcherCrForKyma := createWatcherCR("skr-webhook-manager", true)
	issuer := NewTestIssuer(istioSystemNs)
	kymaObjKey := client.ObjectKeyFromObject(kyma)
	tlsSecret := createTLSSecret(kymaObjKey)
	skrTLSSecretObjKey := client.ObjectKey{Name: watcher.SkrTLSName, Namespace: flags.DefaultRemoteSyncNamespace}
	caCertificate := createCaCertificate()
	caSecret := createCASecret()
	registerDefaultLifecycleForKymaWithWatcher(kyma, watcherCrForKyma, tlsSecret, issuer, caCertificate, caSecret)
	var skrClient client.Client
	var err error
	BeforeAll(func() {
		Eventually(func() error {
			skrClient, err = testSkrContextFactory.Get(kyma.GetNamespacedName())
			return err
		}, Timeout, Interval).Should(Succeed())
	})

	It("remote kyma created on SKR", func() {
		Eventually(KymaExists, Timeout, Interval).
			WithContext(ctx).
			WithArguments(skrClient, shared.DefaultRemoteKymaName, flags.DefaultRemoteSyncNamespace).
			Should(Succeed())
	})

	It("kyma reconciliation creates Certificate CR on KCP", func() {
		Eventually(certificateExists, Timeout, Interval).
			WithArguments(kcpClient, kyma.Name).
			Should(Succeed())

		By("deleting the Certificate CR on KCP")
		certificateCR, err := getCertificate(kcpClient, kyma.Name)
		Expect(err).ToNot(HaveOccurred())
		Eventually(DeleteCR, Timeout, Interval).
			WithContext(ctx).
			WithArguments(skrClient, certificateCR).Should(Succeed())

		By("Certificate CR recreated on KCP")
		Eventually(certificateExists, Timeout, Interval).
			WithArguments(kcpClient, kyma.Name).
			Should(Succeed())
	})

	It("kyma reconciliation creates Certificate Secret on SKR", func() {
		Eventually(secretExists, Timeout, Interval).
			WithArguments(skrClient, skrTLSSecretObjKey).
			Should(Succeed())

		By("deleting the Certificate Secret on SKR")
		secret, err := getSecret(skrClient, skrTLSSecretObjKey)
		Expect(err).ToNot(HaveOccurred())
		Eventually(DeleteCR, Timeout, Interval).
			WithContext(ctx).
			WithArguments(skrClient, secret).Should(Succeed())

		By("recreated Certificate Secret on SKR")
		Eventually(secretExists, Timeout, Interval).
			WithArguments(skrClient, skrTLSSecretObjKey).
			Should(Succeed())
	})

	It("kyma reconciliation updates existing TLS Secret on SKR", func() {
		newKey := "new-pk"

		By("changing the TLS secret on KCP")
		tlsSecret.Data[apicorev1.TLSPrivateKeyKey] = []byte(newKey)
		Expect(kcpClient.Update(ctx, tlsSecret)).To(Succeed())

		By("updates the TLS secret on SKR")
		Eventually(matchSecretPrivateKey, Timeout, Interval).
			WithArguments(skrClient, skrTLSSecretObjKey, []byte(newKey)).
			Should(Succeed())
	})

	AfterAll(func() {
		Expect(kcpClient.Delete(ctx, kyma)).To(Succeed())
	})
})

var _ = Describe("Istio Gateway Certificate Secret Management", Ordered, func() {
	caCertificate := createCaCertificate()
	caSecret := createCASecret()
	BeforeAll(func() {
		By("Creating CA Certificate")
		Expect(kcpClient.Create(ctx, caCertificate)).To(Succeed())
		By("Creating CA Secret")
		Expect(kcpClient.Create(ctx, caSecret)).To(Succeed())
	})
	AfterAll(func() {
		By("Deleting CA Certificate")
		Expect(kcpClient.Delete(ctx, caCertificate)).To(Succeed())
		By("Deleting CA Secret")
		Expect(kcpClient.Delete(ctx, caSecret)).To(Succeed())
	})

	It("istio gateway secret exists", func() {
		Eventually(secretExists, Timeout, Interval).
			WithArguments(kcpClient, client.ObjectKey{Name: "gateway-secret", Namespace: istioSystemNs}).
			Should(Succeed())
	})

	It("istio gateway secret updates when KCP secret rotated", func() {
		Expect(updateCAPrivateKey(kcpClient, "new-private-key")).To(Succeed())
		Eventually(matchSecretPrivateKey, Timeout, Interval).
			WithArguments(kcpClient, client.ObjectKey{Name: "gateway-secret", Namespace: istioSystemNs},
				[]byte("new-private-key")).
			Should(Succeed())
	})
})

func getCertificate(clnt client.Client, kymaName string) (*certmanagerv1.Certificate, error) {
	certificateCR := &certmanagerv1.Certificate{}
	err := clnt.Get(ctx,
		client.ObjectKey{Name: watcher.ResolveTLSCertName(kymaName), Namespace: istioSystemNs},
		certificateCR)
	return certificateCR, err
}

func getSecret(clnt client.Client, objKey client.ObjectKey) (*apicorev1.Secret, error) {
	secretCR := &apicorev1.Secret{}
	err := clnt.Get(ctx, objKey, secretCR)
	return secretCR, err
}

func certificateExists(clnt client.Client, kymaName string) error {
	_, err := getCertificate(clnt, kymaName)
	if util.IsNotFound(err) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return nil
}

func secretExists(clnt client.Client, secretObjKey client.ObjectKey) error {
	_, err := getSecret(clnt, secretObjKey)
	if util.IsNotFound(err) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return nil
}

func updateCAPrivateKey(clnt client.Client, newPrivateKeyValue string) error {
	caCert, err := getSecret(clnt, types.NamespacedName{Name: "klm-watcher", Namespace: istioSystemNs})
	if err != nil {
		return fmt.Errorf("error getting CA secret: %w", err)
	}
	caCert.Data[apicorev1.TLSPrivateKeyKey] = []byte(newPrivateKeyValue)
	if err := clnt.Update(ctx, caCert); err != nil {
		return fmt.Errorf("error updating CA secret: %w", err)
	}
	return nil
}

func matchSecretPrivateKey(clnt client.Client, secretObjKey client.ObjectKey, privateKey []byte) error {
	secretCR, err := getSecret(clnt, secretObjKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(secretCR.Data[apicorev1.TLSPrivateKeyKey], privateKey) {
		return ErrPrivateKeyNotMatching
	}
	return nil
}
