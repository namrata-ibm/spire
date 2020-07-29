package attestor_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus/hooks/test"
	attestor "github.com/spiffe/spire/pkg/agent/attestor/node"
	"github.com/spiffe/spire/pkg/agent/plugin/keymanager"
	"github.com/spiffe/spire/pkg/agent/plugin/keymanager/memory"
	agentnodeattestor "github.com/spiffe/spire/pkg/agent/plugin/nodeattestor"
	"github.com/spiffe/spire/pkg/common/catalog"
	"github.com/spiffe/spire/pkg/common/idutil"
	"github.com/spiffe/spire/pkg/common/pemutil"
	"github.com/spiffe/spire/pkg/common/telemetry"
	servernodeattestor "github.com/spiffe/spire/pkg/server/plugin/nodeattestor"
	"github.com/spiffe/spire/proto/spire/api/node"
	"github.com/spiffe/spire/proto/spire/common"
	"github.com/spiffe/spire/test/fakes/fakeagentcatalog"
	"github.com/spiffe/spire/test/fakes/fakeagentnodeattestor"
	"github.com/spiffe/spire/test/fakes/fakeservernodeattestor"
	"github.com/spiffe/spire/test/spiretest"
	"github.com/stretchr/testify/require"
)

var (
	testKey, _ = pemutil.ParseSigner([]byte(`-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgy8ps3oQaBaSUFpfd
XM13o+VSA0tcZteyTvbOdIQNVnKhRANCAAT4dPIORBjghpL5O4h+9kyzZZUAFV9F
qNV3lKIL59N7G2B4ojbhfSNneSIIpP448uPxUnaunaQZ+/m7+x9oobIp
-----END PRIVATE KEY-----
`))
)

func TestAttestor(t *testing.T) {
	// create CA and server certificates
	caCert := createCACertificate(t)
	serverCert := createServerCertificate(t, caCert)
	agentCert := createAgentCertificate(t, caCert, "/test/foo")
	expiredCert := createExpiredCertificate(t, caCert)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{
			{
				Certificate: [][]byte{serverCert.Raw},
				PrivateKey:  testKey,
			},
		},
	}

	testCases := []struct {
		name                        string
		challengeResponses          []string
		bootstrapBundle             *x509.Certificate
		insecureBootstrap           bool
		cachedBundle                []byte
		cachedSVID                  []byte
		joinToken                   string
		err                         string
		omitSVIDUpdate              bool
		overrideSVIDUpdate          *node.X509SVIDUpdate
		storeKey                    crypto.PrivateKey
		failFetchingAttestationData bool
		failAttestCall              bool
	}{
		{
			name:              "insecure bootstrap",
			insecureBootstrap: true,
		},
		{
			name:         "cached bundle empty",
			cachedBundle: []byte(""),
			err:          "load bundle: no certs in bundle",
		},
		{
			name:         "cached bundle malformed",
			cachedBundle: []byte("INVALID DER BYTES"),
			err:          "load bundle: error parsing bundle",
		},
		{
			name:                        "fail fetching attestation data",
			bootstrapBundle:             caCert,
			err:                         "fetching attestation data purposefully failed",
			failFetchingAttestationData: true,
		},
		{
			name:            "response missing svid update",
			bootstrapBundle: caCert,
			omitSVIDUpdate:  true,
			err:             "failed to parse attestation response: missing svid update",
		},
		{
			name:            "response has more than one svid",
			bootstrapBundle: caCert,
			overrideSVIDUpdate: &node.X509SVIDUpdate{
				Svids: map[string]*node.X509SVID{
					"spiffe://domain.test/not/used":      {},
					"spiffe://domain.test/also/not/used": {},
				},
			},
			err: "failed to parse attestation response: expected 1 svid; got 2",
		},
		{
			name:            "response svid has invalid cert chain",
			bootstrapBundle: caCert,
			overrideSVIDUpdate: &node.X509SVIDUpdate{
				Svids: map[string]*node.X509SVID{
					"spiffe://domain.test/not/used": {CertChain: []byte("INVALID")},
				},
			},
			err: "failed to parse attestation response: invalid svid cert chain",
		},
		{
			name:            "response svid has empty cert chain",
			bootstrapBundle: caCert,
			overrideSVIDUpdate: &node.X509SVIDUpdate{
				Svids: map[string]*node.X509SVID{
					"spiffe://domain.test/not/used": {},
				},
			},
			err: "failed to parse attestation response: empty svid cert chain",
		},
		{
			name:            "response missing trust domain bundle",
			bootstrapBundle: caCert,
			overrideSVIDUpdate: &node.X509SVIDUpdate{
				Svids: map[string]*node.X509SVID{
					"spiffe://domain.test/not/used": {CertChain: agentCert.Raw},
				},
			},
			err: "failed to parse attestation response: missing trust domain bundle",
		},
		{
			name:            "response has malformed trust domain bundle",
			bootstrapBundle: caCert,
			overrideSVIDUpdate: &node.X509SVIDUpdate{
				Svids: map[string]*node.X509SVID{
					"spiffe://domain.test/not/used": {CertChain: agentCert.Raw},
				},
				Bundles: map[string]*common.Bundle{
					"spiffe://domain.test": {
						RootCas: []*common.Certificate{
							{DerBytes: []byte("INVALID")},
						},
					},
				},
			},
			err: "failed to parse attestation response: invalid trust domain bundle",
		},
		{
			name:            "success with bootstrap bundle",
			bootstrapBundle: caCert,
		},
		{
			name:         "success with cached bundle",
			cachedBundle: caCert.Raw,
		},
		{
			name:            "success with expired cached bundle",
			bootstrapBundle: caCert,
			cachedSVID:      expiredCert.Raw,
		},
		{
			name:            "success with join token",
			bootstrapBundle: caCert,
			joinToken:       "JOINTOKEN",
		},
		{
			name:               "success with challenge response",
			bootstrapBundle:    caCert,
			challengeResponses: []string{"FOO", "BAR", "BAZ"},
		},
		{
			name:              "cached svid and private key but missing bundle",
			insecureBootstrap: true,
			cachedSVID:        agentCert.Raw,
			storeKey:          testKey,
			err:               "SVID loaded but no bundle in cache",
		},
		{
			name:           "success with cached svid, private key, and bundle",
			cachedBundle:   caCert.Raw,
			cachedSVID:     agentCert.Raw,
			storeKey:       testKey,
			failAttestCall: true,
		},
		{
			name:            "malformed cached svid ignored",
			bootstrapBundle: caCert,
			cachedSVID:      []byte("INVALID"),
			storeKey:        testKey,
			failAttestCall:  true,
			err:             "attestation has been purposefully failed",
		},
		{
			name:            "missing key in keymanager ignored",
			bootstrapBundle: caCert,
			cachedSVID:      agentCert.Raw,
			failAttestCall:  true,
			err:             "attestation has been purposefully failed",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			require := require.New(t)

			// prepare the temp directory holding the cached bundle/svid
			svidCachePath, bundleCachePath := prepareTestDir(t, testCase.cachedSVID, testCase.cachedBundle)

			// load up the fake agent-side node attestor
			agentNA := prepareAgentNA(t, fakeagentnodeattestor.Config{
				Fail:      testCase.failFetchingAttestationData,
				Responses: testCase.challengeResponses,
			})

			// load up the fake server-side node attestor
			serverNA := prepareServerNA(t, fakeservernodeattestor.Config{
				TrustDomain: "domain.test",
				Data: map[string]string{
					"TEST": "foo",
				},
				Challenges: map[string][]string{
					"foo": testCase.challengeResponses,
				},
			})

			// load up an in-memory key manager
			km := prepareKeyManager(t, testCase.storeKey)

			// initialize the catalog
			catalog := fakeagentcatalog.New()
			catalog.SetNodeAttestor(fakeagentcatalog.NodeAttestor("test", agentNA))
			catalog.SetKeyManager(fakeagentcatalog.KeyManager(km))

			// kick off the gRPC server serving the node API
			serverAddr, serverDone := startNodeServer(t, tlsConfig, fakeNodeAPIConfig{
				CACert:             caCert,
				Attestor:           serverNA,
				OmitSVIDUpdate:     testCase.omitSVIDUpdate,
				OverrideSVIDUpdate: testCase.overrideSVIDUpdate,
				FailAttestCall:     testCase.failAttestCall,
			})
			defer serverDone()

			// create the attestor
			log, _ := test.NewNullLogger()
			attestor := attestor.New(&attestor.Config{
				Catalog:         catalog,
				Metrics:         telemetry.Blackhole{},
				JoinToken:       testCase.joinToken,
				SVIDCachePath:   svidCachePath,
				BundleCachePath: bundleCachePath,
				Log:             log,
				TrustDomain: url.URL{
					Scheme: "spiffe",
					Host:   "domain.test",
				},
				TrustBundle:       makeTrustBundle(testCase.bootstrapBundle),
				InsecureBootstrap: testCase.insecureBootstrap,
				ServerAddress:     serverAddr,
			})

			// perform attestation
			result, err := attestor.Attest(context.Background())
			if testCase.err != "" {
				spiretest.RequireErrorContains(t, err, testCase.err)
				return
			}
			require.NoError(err)
			require.NotNil(result)
			require.Len(result.SVID, 1)
			require.Len(result.SVID[0].URIs, 1)
			if testCase.joinToken != "" {
				require.Equal("spiffe://domain.test/spire/agent/join_token/"+testCase.joinToken, result.SVID[0].URIs[0].String())
			} else {
				require.Equal("spiffe://domain.test/spire/agent/test/foo", result.SVID[0].URIs[0].String())
			}
			require.NotNil(result.Key)
			require.NotNil(result.Bundle)

			rootCAs := result.Bundle.RootCAs()
			require.Len(rootCAs, 1)
			require.Equal(rootCAs[0].Raw, caCert.Raw)
		})
	}
}

func prepareTestDir(t *testing.T, cachedSVID, cachedBundle []byte) (string, string) {
	dir := spiretest.TempDir(t)

	svidCachePath := filepath.Join(dir, "svid.der")
	bundleCachePath := filepath.Join(dir, "bundle.der")
	if cachedSVID != nil {
		writeFile(t, svidCachePath, cachedSVID, 0644)
	}
	if cachedBundle != nil {
		writeFile(t, bundleCachePath, cachedBundle, 0644)
	}

	return svidCachePath, bundleCachePath
}

func prepareAgentNA(t *testing.T, config fakeagentnodeattestor.Config) agentnodeattestor.NodeAttestor {
	var agentNA agentnodeattestor.NodeAttestor
	spiretest.LoadPlugin(t, catalog.MakePlugin("test",
		agentnodeattestor.PluginServer(fakeagentnodeattestor.New(config)),
	), &agentNA)
	return agentNA
}

func prepareServerNA(t *testing.T, config fakeservernodeattestor.Config) servernodeattestor.NodeAttestor {
	var serverNA servernodeattestor.NodeAttestor
	spiretest.LoadPlugin(t, catalog.MakePlugin("test",
		servernodeattestor.PluginServer(fakeservernodeattestor.New("test", config)),
	), &serverNA)
	return serverNA
}

func prepareKeyManager(t *testing.T, key crypto.PrivateKey) keymanager.KeyManager {
	var km keymanager.KeyManager
	spiretest.LoadPlugin(t, memory.BuiltIn(), &km)
	if key != nil {
		storePrivateKey(t, km, key)
	}
	return km
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	require.NoError(t, ioutil.WriteFile(path, data, mode))
}

func createCACertificate(t *testing.T) *x509.Certificate {
	tmpl := &x509.Certificate{
		BasicConstraintsValid: true,
		IsCA:                  true,
		URIs:                  []*url.URL{idutil.TrustDomainURI("domain.test")},
	}
	return createCertificate(t, tmpl, tmpl)
}

func createServerCertificate(t *testing.T, caCert *x509.Certificate) *x509.Certificate {
	tmpl := &x509.Certificate{
		URIs:     []*url.URL{idutil.ServerURI("domain.test")},
		DNSNames: []string{"localhost"},
	}
	return createCertificate(t, tmpl, caCert)
}

func createAgentCertificate(t *testing.T, caCert *x509.Certificate, path string) *x509.Certificate {
	tmpl := &x509.Certificate{
		URIs: []*url.URL{idutil.AgentURI("domain.test", path)},
	}
	return createCertificate(t, tmpl, caCert)
}

func createExpiredCertificate(t *testing.T, caCert *x509.Certificate) *x509.Certificate {
	tmpl := &x509.Certificate{
		NotAfter: time.Now().Add(-1 * time.Hour),
		URIs:     []*url.URL{idutil.AgentURI("domain.test", "/test/expired")},
	}
	return createCertificate(t, tmpl, caCert)
}

func createCertificate(t *testing.T, tmpl, parent *x509.Certificate) *x509.Certificate {
	now := time.Now()
	tmpl.SerialNumber = big.NewInt(0)
	tmpl.NotBefore = now
	if tmpl.NotAfter.IsZero() {
		tmpl.NotAfter = now.Add(time.Hour)
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, parent, testKey.Public(), testKey)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(certDER)
	require.NoError(t, err)
	return cert
}

func storePrivateKey(t *testing.T, km keymanager.KeyManager, privateKey crypto.PrivateKey) {
	ecKey, ok := privateKey.(*ecdsa.PrivateKey)
	require.True(t, ok, "not an EC key")
	keyBytes, err := x509.MarshalECPrivateKey(ecKey)
	require.NoError(t, err)
	_, err = km.StorePrivateKey(context.Background(), &keymanager.StorePrivateKeyRequest{
		PrivateKey: keyBytes,
	})
	require.NoError(t, err)
}

func makeTrustBundle(bootstrapCert *x509.Certificate) []*x509.Certificate {
	var trustBundle []*x509.Certificate
	if bootstrapCert != nil {
		trustBundle = append(trustBundle, bootstrapCert)
	}
	return trustBundle
}

func TestIsSVIDExpired(t *testing.T) {
	now := time.Now()

	tests := []struct {
		Desc          string
		SVID          []*x509.Certificate
		ExpectExpired bool
	}{
		{
			Desc: "cert expiration is in the past",
			SVID: []*x509.Certificate{
				{NotAfter: now.Add(-2 * time.Second)},
			},
			ExpectExpired: true,
		},
		{
			Desc: "cert is about to expire",
			SVID: []*x509.Certificate{
				{NotAfter: now.Add(time.Second)},
			},
			ExpectExpired: true,
		},
		{
			Desc: "cert expiration is safely in the future",
			SVID: []*x509.Certificate{
				{NotAfter: now.Add(time.Minute)},
			},
			ExpectExpired: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.Desc, func(t *testing.T) {
			isExpired := attestor.IsSVIDExpired(tt.SVID, func() time.Time { return now })
			require.Equal(t, tt.ExpectExpired, isExpired)
		})
	}
}
