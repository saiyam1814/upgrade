package crds

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/saiyam1814/upgrade/internal/finding"
)

// ---- check 2: cert-expiry parsing ----

func TestCheckConversionWebhookCert_NoConversion(t *testing.T) {
	crd := &apiextv1.CustomResourceDefinition{}
	if got := checkConversionWebhookCert(crd); got != nil {
		t.Errorf("CRD with no conversion strategy should return nil; got %+v", got)
	}
}

func TestCheckConversionWebhookCert_NonWebhookStrategy(t *testing.T) {
	crd := &apiextv1.CustomResourceDefinition{
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Conversion: &apiextv1.CustomResourceConversion{Strategy: apiextv1.NoneConverter},
		},
	}
	if got := checkConversionWebhookCert(crd); got != nil {
		t.Errorf("CRD with NoneConverter should return nil; got %+v", got)
	}
}

func TestCheckConversionWebhookCert_EmptyCABundle(t *testing.T) {
	crd := &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Conversion: &apiextv1.CustomResourceConversion{
				Strategy: apiextv1.WebhookConverter,
				Webhook: &apiextv1.WebhookConversion{
					ClientConfig: &apiextv1.WebhookClientConfig{},
				},
			},
		},
	}
	got := checkConversionWebhookCert(crd)
	if got == nil {
		t.Fatal("empty caBundle should produce a finding")
	}
	if got.Severity != finding.High {
		t.Errorf("expected HIGH for empty caBundle; got %s", got.Severity)
	}
}

func TestCheckConversionWebhookCert_ExpiringIn5Days_IsBlocker(t *testing.T) {
	cert := mintCert(t, 5*24*time.Hour)
	crd := webhookCRDWithCABundle("widgets.example.com", cert)
	got := checkConversionWebhookCert(crd)
	if got == nil || got.Severity != finding.Blocker {
		t.Errorf("expected BLOCKER for cert expiring in 5 days; got %+v", got)
	}
}

func TestCheckConversionWebhookCert_ExpiringIn20Days_IsHigh(t *testing.T) {
	cert := mintCert(t, 20*24*time.Hour)
	crd := webhookCRDWithCABundle("widgets.example.com", cert)
	got := checkConversionWebhookCert(crd)
	if got == nil || got.Severity != finding.High {
		t.Errorf("expected HIGH for cert expiring in 20 days; got %+v", got)
	}
}

func TestCheckConversionWebhookCert_ExpiringIn90Days_NoFinding(t *testing.T) {
	cert := mintCert(t, 90*24*time.Hour)
	crd := webhookCRDWithCABundle("widgets.example.com", cert)
	if got := checkConversionWebhookCert(crd); got != nil {
		t.Errorf("expected nil for cert expiring in 90 days; got %+v", got)
	}
}

func TestCheckConversionWebhookCert_AlreadyExpired_IsBlocker(t *testing.T) {
	cert := mintCert(t, -2*24*time.Hour) // 2 days ago
	crd := webhookCRDWithCABundle("widgets.example.com", cert)
	got := checkConversionWebhookCert(crd)
	if got == nil || got.Severity != finding.Blocker {
		t.Errorf("expected BLOCKER for expired cert; got %+v", got)
	}
}

func TestCheckConversionWebhookCert_GarbageCABundle(t *testing.T) {
	crd := &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Conversion: &apiextv1.CustomResourceConversion{
				Strategy: apiextv1.WebhookConverter,
				Webhook: &apiextv1.WebhookConversion{
					ClientConfig: &apiextv1.WebhookClientConfig{
						CABundle: []byte("not a cert"),
					},
				},
			},
		},
	}
	got := checkConversionWebhookCert(crd)
	if got == nil || got.Severity != finding.Medium {
		t.Errorf("expected MEDIUM for unparseable caBundle; got %+v", got)
	}
}

// ---- check 3: orphan detection ----

func TestIdentifyOwner_HelmManaged(t *testing.T) {
	crd := &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"},
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      "cert-manager",
				"meta.helm.sh/release-namespace": "cert-manager",
			},
		},
	}
	if got := identifyOwner(crd); got != "cert-manager/cert-manager" {
		t.Errorf("got %q want cert-manager/cert-manager", got)
	}
}

func TestIdentifyOwner_NoLabel(t *testing.T) {
	crd := &apiextv1.CustomResourceDefinition{}
	if got := identifyOwner(crd); got != "" {
		t.Errorf("unlabeled CRD should return empty; got %q", got)
	}
}

func TestLoadInstalledControllers_FindsHelmRelease(t *testing.T) {
	core := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sh.helm.release.v1.cert-manager.v1",
				Namespace: "cert-manager",
				Labels:    map[string]string{"owner": "helm", "name": "cert-manager"},
			},
			Type: corev1.SecretType("helm.sh/release.v1"),
		},
	)
	got := loadInstalledControllers(context.Background(), core)
	if !got["cert-manager/cert-manager"] {
		t.Errorf("expected cert-manager/cert-manager in installed set; got %v", got)
	}
	if !got["cert-manager"] {
		t.Errorf("expected bare-name cert-manager in installed set; got %v", got)
	}
}

// ---- helpers ----

// mintCert produces a self-signed X.509 cert with NotAfter = now + d.
// Returns the cert wrapped in PEM (the canonical caBundle format).
func mintCert(t *testing.T, d time.Duration) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(d),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func webhookCRDWithCABundle(name string, ca []byte) *apiextv1.CustomResourceDefinition {
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Conversion: &apiextv1.CustomResourceConversion{
				Strategy: apiextv1.WebhookConverter,
				Webhook: &apiextv1.WebhookConversion{
					ClientConfig: &apiextv1.WebhookClientConfig{CABundle: ca},
				},
			},
		},
	}
}
