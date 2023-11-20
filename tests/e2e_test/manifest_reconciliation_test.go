package e2e_test

import (
	"github.com/kyma-project/lifecycle-manager/api/shared"
	"github.com/kyma-project/lifecycle-manager/api/v1beta2"
	v2 "github.com/kyma-project/lifecycle-manager/internal/declarative/v2"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/kyma-project/lifecycle-manager/pkg/testutils"
)

var _ = Describe("Manifest Skip Reconciliation Label", Ordered, func() {
	kyma := NewKymaWithSyncLabel("kyma-sample", "kcp-system", v1beta2.DefaultChannel,
		v1beta2.SyncStrategyLocalSecret)
	module := NewTemplateOperator(v1beta2.DefaultChannel)

	InitEmptyKymaBeforeAll(kyma)
	CleanupKymaAfterAll(kyma)

	Context("Given kyma deployed in KCP", func() {
		It("When enabling Template Operator", func() {
			Eventually(EnableModule).
				WithContext(ctx).
				WithArguments(runtimeClient, defaultRemoteKymaName, remoteNamespace, module).
				Should(Succeed())
			By("Then the Module Operator is deployed on the SKR cluster")
			Eventually(CheckIfExists).
				WithContext(ctx).
				WithArguments("template-operator-controller-manager", "template-operator-system", "apps", "v1",
					"Deployment", runtimeClient).
				Should(Succeed())
			By("And the SKR Module Default CR is in a \"Ready\" State")
			Eventually(CheckSampleCRIsInState).
				WithContext(ctx).
				WithArguments("sample-yaml", "kyma-system", runtimeClient, "Ready").
				Should(Succeed())
			By("And the KCP Kyma CR is in a \"Ready\" State")
			Eventually(KymaIsInState).
				WithContext(ctx).
				WithArguments(kyma.GetName(), kyma.GetNamespace(), controlPlaneClient, shared.StateReady).
				Should(Succeed())
		})

		It("When the Manifest is labelled to skip reconciliation", func() {
			labels, err := GetManifestLabels(ctx, kyma.GetName(), kyma.GetNamespace(), module.Name, controlPlaneClient)
			Expect(err).ToNot(HaveOccurred())
			labels[v2.SkipReconcileLabel] = "true"
			err = SetManifestLabels(ctx, kyma.GetName(), kyma.GetNamespace(), module.Name, controlPlaneClient, labels)
			Expect(err).ToNot(HaveOccurred())

			Eventually(DisableModule).
				WithContext(ctx).
				WithArguments(runtimeClient, defaultRemoteKymaName, remoteNamespace, module.Name).
				Should(Succeed())
			By("Then the KCP Kyma CR is in a \"Processing\" State")
			Eventually(KymaIsInState).
				WithContext(ctx).
				WithArguments(kyma.GetName(), kyma.GetNamespace(), controlPlaneClient, shared.StateProcessing).
				Should(Succeed())
			By("And the Module Manifest CR is in a \"Deleting\" State")
			Eventually(CheckManifestIsInState).
				WithContext(ctx).
				WithArguments(kyma.GetName(), kyma.GetNamespace(), module.Name, controlPlaneClient, shared.StateDeleting).
				Should(Succeed())
			By("And the SKR Module Default CR is not removed")
			Consistently(CheckIfExists).
				WithContext(ctx).
				WithArguments("sample-yaml", "kyma-system", "operator.kyma-project.io",
					"v1alpha1", "Sample", runtimeClient).
				Should(Succeed())
			By("And the SKR Module Default CR is in a \"Deleting\" State")
			Consistently(CheckSampleCRIsInState).
				WithContext(ctx).
				WithArguments("sample-yaml", "kyma-system", runtimeClient, "Deleting").
				Should(Succeed())
			Eventually(SampleCRDeletionTimeStampSet).
				WithContext(ctx).
				WithArguments("sample-yaml", "kyma-system", runtimeClient).
				Should(Succeed())

			By("And the Module Manager Deployment is not removed on the SKR cluster")
			Consistently(CheckIfExists).
				WithContext(ctx).
				WithArguments("template-operator-controller-manager", "template-operator-system",
					"apps", "v1", "Deployment", runtimeClient).
				Should(Succeed())
		})

		It("When the Manifest skip reconciliation label removed",
			func() {
				labels, err := GetManifestLabels(ctx, kyma.GetName(), kyma.GetNamespace(), module.Name, controlPlaneClient)
				Expect(err).ToNot(HaveOccurred())
				delete(labels, v2.SkipReconcileLabel)
				err = SetManifestLabels(ctx, kyma.GetName(), kyma.GetNamespace(), module.Name, controlPlaneClient, labels)
				Expect(err).ToNot(HaveOccurred())

				By("Then the Module Default CR is removed")
				Eventually(CheckIfNotExists).
					WithContext(ctx).
					WithArguments("sample-yaml", "kyma-system",
						"operator.kyma-project.io", "v1alpha1", "Sample", runtimeClient).
					Should(Succeed())
				By("And the Module Deployment is deleted")
				Eventually(CheckIfNotExists).
					WithContext(ctx).
					WithArguments("template-operator-v2-controller-manager",
						"template-operator-system", "apps", "v1", "Deployment", runtimeClient).
					Should(Succeed())
				By("And the Manifest CR is removed")
				Eventually(NoManifestExist).
					WithContext(ctx).
					WithArguments(controlPlaneClient).
					Should(Succeed())

				By("And the KCP Kyma CR is in a \"Ready\" State")
				Eventually(KymaIsInState).
					WithContext(ctx).
					WithArguments(kyma.GetName(), kyma.GetNamespace(), controlPlaneClient, shared.StateReady).
					Should(Succeed())

				By("And the SKR Kyma CR is in a \"Ready\" State")
				Eventually(KymaIsInState).
					WithContext(ctx).
					WithArguments(defaultRemoteKymaName, remoteNamespace, runtimeClient, shared.StateReady).
					Should(Succeed())
			})
	})
})
