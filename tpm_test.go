// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package secboot_test

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/canonical/go-tpm2"
	. "github.com/snapcore/secboot"
	"github.com/snapcore/snapd/snap"
)

var (
	useTpm         = flag.Bool("use-tpm", false, "")
	tpmPathForTest = flag.String("tpm-path", "/dev/tpm0", "")

	useMssim  = flag.Bool("use-mssim", false, "")
	mssimPort = flag.Uint("mssim-port", 2321, "")

	testCACert []byte
	testCAKey  crypto.PrivateKey

	testEkCert []byte

	testEncodedEkCertChain []byte
)

func decodeHexStringT(t *testing.T, s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("DecodeHexString failed: %v", err)
	}
	return b
}

// Flush a handle context. Fails the test if it doesn't succeed.
func flushContext(t *testing.T, tpm *TPMConnection, context tpm2.HandleContext) {
	if err := tpm.FlushContext(context); err != nil {
		t.Errorf("FlushContext failed: %v", err)
	}
}

// Set the hierarchy auth to testAuth. Fatal on failure
func setHierarchyAuthForTest(t *testing.T, tpm *TPMConnection, hierarchy tpm2.ResourceContext) {
	if err := tpm.HierarchyChangeAuth(hierarchy, tpm2.Auth(testAuth), nil); err != nil {
		t.Fatalf("HierarchyChangeAuth failed: %v", err)
	}
}

// Reset the hierarchy auth to nil.
func resetHierarchyAuth(t *testing.T, tpm *TPMConnection, hierarchy tpm2.ResourceContext) {
	if err := tpm.HierarchyChangeAuth(hierarchy, nil, nil); err != nil {
		t.Errorf("HierarchyChangeAuth failed: %v", err)
	}
}

// Undefine a NV index set by a test. Fails the test if it doesn't succeed.
func undefineNVSpace(t *testing.T, tpm *TPMConnection, context, authHandle tpm2.ResourceContext) {
	if err := tpm.NVUndefineSpace(authHandle, context, nil); err != nil {
		t.Errorf("NVUndefineSpace failed: %v", err)
	}
}

func undefineKeyNVSpace(t *testing.T, tpm *TPMConnection, path string) {
	k, err := ReadSealedKeyObject(path)
	if err != nil {
		t.Fatalf("ReadSealedKeyObject failed: %v", err)
	}
	rc, err := tpm.CreateResourceContextFromTPM(k.PINIndexHandle())
	if err != nil {
		t.Fatalf("CreateResourceContextFromTPM failed: %v", err)
	}
	undefineNVSpace(t, tpm, rc, tpm.OwnerHandleContext())
}

func openTPMSimulatorForTestingCommon() (*TPMConnection, *tpm2.TctiMssim, error) {
	if !*useMssim {
		return nil, nil, nil
	}

	if *useTpm && *useMssim {
		return nil, nil, errors.New("cannot specify both -use-tpm and -use-mssim")
	}

	var tcti *tpm2.TctiMssim

	SetOpenDefaultTctiFn(func() (io.ReadWriteCloser, error) {
		var err error
		tcti, err = tpm2.OpenMssim("", *mssimPort, *mssimPort+1)
		if err != nil {
			return nil, err
		}
		return tcti, nil
	})

	tpm, err := SecureConnectToDefaultTPM(bytes.NewReader(testEncodedEkCertChain), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("ConnectToDefaultTPM failed: %v", err)
	}

	return tpm, tcti, nil
}

func openTPMSimulatorForTesting(t *testing.T) (*TPMConnection, *tpm2.TctiMssim) {
	tpm, tcti, err := openTPMSimulatorForTestingCommon()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if tpm == nil {
		t.SkipNow()
	}
	return tpm, tcti
}

func openTPMForTestingCommon() (*TPMConnection, error) {
	if !*useTpm {
		tpm, _, err := openTPMSimulatorForTestingCommon()
		return tpm, err
	}

	if *useTpm && *useMssim {
		return nil, errors.New("cannot specify both -use-tpm and -use-mssim")
	}

	SetOpenDefaultTctiFn(func() (io.ReadWriteCloser, error) {
		return tpm2.OpenTPMDevice(*tpmPathForTest)
	})

	tpm, err := ConnectToDefaultTPM()
	if err != nil {
		return nil, fmt.Errorf("ConnectToDefaultTPM failed: %v", err)
	}

	return tpm, nil
}

func openTPMForTesting(t *testing.T) *TPMConnection {
	tpm, err := openTPMForTestingCommon()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if tpm == nil {
		t.SkipNow()
	}
	return tpm
}

// clearTPMWithPlatformAuth clears the TPM with platform hierarchy authorization - something that we can only do on the simulator
func clearTPMWithPlatformAuth(t *testing.T, tpm *TPMConnection) {
	if err := tpm.ClearControl(tpm.PlatformHandleContext(), false, nil); err != nil {
		t.Fatalf("ClearControl failed: %v", err)
	}
	if err := tpm.Clear(tpm.PlatformHandleContext(), nil); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}
}

func resetTPMSimulatorCommon(tpm *TPMConnection, tcti *tpm2.TctiMssim) error {
	if err := tpm.Shutdown(tpm2.StartupClear); err != nil {
		return fmt.Errorf("Shutdown failed: %v", err)
	}
	if err := tcti.Reset(); err != nil {
		return fmt.Errorf("resetting the TPM simulator failed: %v", err)
	}
	if err := tpm.Startup(tpm2.StartupClear); err != nil {
		return fmt.Errorf("Startup failed: %v", err)
	}

	if err := InitTPMConnection(tpm); err != nil {
		return fmt.Errorf("failed to reinitialize TPMConnection after reset: %v", err)
	}
	return nil
}

// resetTPMSimulator executes reset sequence of the TPM (Shutdown(CLEAR) -> reset -> Startup(CLEAR)) and the re-initializes the
// TPMConnection.
func resetTPMSimulator(t *testing.T, tpm *TPMConnection, tcti *tpm2.TctiMssim) {
	if err := resetTPMSimulatorCommon(tpm, tcti); err != nil {
		t.Fatalf("%v", err)
	}
}

func closeTPM(t *testing.T, tpm *TPMConnection) {
	if err := tpm.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func createTestCA() ([]byte, crypto.PrivateKey, error) {
	serial := big.NewInt(rand.Int63())

	keyId := make([]byte, 32)
	if _, err := rand.Read(keyId); err != nil {
		return nil, nil, fmt.Errorf("cannot obtain random key ID: %v", err)
	}

	key, err := rsa.GenerateKey(testRandReader, 768)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot generate RSA key: %v", err)
	}

	t := time.Now()

	template := x509.Certificate{
		SignatureAlgorithm: x509.SHA256WithRSA,
		SerialNumber:       serial,
		Subject: pkix.Name{
			Country:      []string{"US"},
			Organization: []string{"Snake Oil TPM Manufacturer"},
			CommonName:   "Snake Oil TPM Manufacturer EK Root CA"},
		NotBefore:             t.Add(time.Hour * -24),
		NotAfter:              t.Add(time.Hour * 240),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		SubjectKeyId:          keyId}

	cert, err := x509.CreateCertificate(testRandReader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create certificate: %v", err)
	}

	return cert, key, nil
}

func createTestEkCert(tpm *tpm2.TPMContext, caCert []byte, caKey crypto.PrivateKey) ([]byte, error) {
	ekContext, pub, _, _, _, err := tpm.CreatePrimary(tpm.EndorsementHandleContext(), nil, EkTemplate, nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot create EK: %v", err)
	}
	defer tpm.FlushContext(ekContext)

	serial := big.NewInt(rand.Int63())

	key := rsa.PublicKey{
		N: new(big.Int).SetBytes(pub.Unique.RSA()),
		E: 65537}

	keyId := make([]byte, 32)
	if _, err := rand.Read(keyId); err != nil {
		return nil, fmt.Errorf("cannot obtain random key ID for EK cert: %v", err)
	}

	t := time.Now()

	tpmDeviceAttrValues := pkix.RDNSequence{
		pkix.RelativeDistinguishedNameSET{
			pkix.AttributeTypeAndValue{Type: OidTcgAttributeTpmManufacturer, Value: "id:49424d00"},
			pkix.AttributeTypeAndValue{Type: OidTcgAttributeTpmModel, Value: "FakeTPM"},
			pkix.AttributeTypeAndValue{Type: OidTcgAttributeTpmVersion, Value: "id:00010002"}}}
	tpmDeviceAttrData, err := asn1.Marshal(tpmDeviceAttrValues)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal SAN value: %v", err)
	}
	sanData, err := asn1.Marshal([]asn1.RawValue{
		asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: SanDirectoryNameTag, IsCompound: true, Bytes: tpmDeviceAttrData}})
	if err != nil {
		return nil, fmt.Errorf("cannot marshal SAN value: %v", err)
	}
	sanExtension := pkix.Extension{
		Id:       OidExtensionSubjectAltName,
		Critical: true,
		Value:    sanData}

	template := x509.Certificate{
		SignatureAlgorithm:    x509.SHA256WithRSA,
		SerialNumber:          serial,
		NotBefore:             t.Add(time.Hour * -24),
		NotAfter:              t.Add(time.Hour * 240),
		KeyUsage:              x509.KeyUsageKeyEncipherment,
		UnknownExtKeyUsage:    []asn1.ObjectIdentifier{OidTcgKpEkCertificate},
		BasicConstraintsValid: true,
		IsCA:                  false,
		SubjectKeyId:          keyId,
		ExtraExtensions:       []pkix.Extension{sanExtension}}

	root, err := x509.ParseCertificate(caCert)
	if err != nil {
		return nil, fmt.Errorf("cannot parse CA certificate: %v", err)
	}

	cert, err := x509.CreateCertificate(testRandReader, &template, root, &key, caKey)
	if err != nil {
		return nil, fmt.Errorf("cannot create EK certificate: %v", err)
	}

	return cert, nil
}

func certifyTPM(tpm *tpm2.TPMContext) error {
	if cert, err := createTestEkCert(tpm, testCACert, testCAKey); err != nil {
		return err
	} else {
		caCert, _ := x509.ParseCertificate(testCACert)
		b := new(bytes.Buffer)
		if err := EncodeEKCertificateChain(nil, []*x509.Certificate{caCert}, b); err != nil {
			return fmt.Errorf("cannot encode EK certificate chain: %v", err)
		}
		testEkCert = cert
		testEncodedEkCertChain = b.Bytes()

		nvPub := tpm2.NVPublic{
			Index:   EkCertHandle,
			NameAlg: tpm2.HashAlgorithmSHA256,
			Attrs:   tpm2.NVTypeOrdinary.WithAttrs(tpm2.AttrNVPPWrite | tpm2.AttrNVAuthRead | tpm2.AttrNVNoDA | tpm2.AttrNVPlatformCreate),
			Size:    uint16(len(cert))}
		index, err := tpm.NVDefineSpace(tpm.PlatformHandleContext(), nil, &nvPub, nil)
		if err != nil {
			return fmt.Errorf("cannot define NV index for EK certificate: %v", err)
		}
		if err := tpm.NVWrite(tpm.PlatformHandleContext(), index, tpm2.MaxNVBuffer(cert), 0, nil); err != nil {
			return fmt.Errorf("cannot write EK certificate to NV index: %v", err)
		}
	}
	return nil
}

func TestTPMConnectionIsEnabled(t *testing.T) {
	tpm, _ := openTPMSimulatorForTesting(t)
	defer func() {
		clearTPMWithPlatformAuth(t, tpm)
		closeTPM(t, tpm)
	}()

	if !tpm.IsEnabled() {
		t.Errorf("IsEnabled returned the wrong value")
	}

	hierarchyControl := func(auth tpm2.ResourceContext, hierarchy tpm2.Handle, enable bool) {
		if err := tpm.HierarchyControl(auth, hierarchy, enable, nil); err != nil {
			t.Errorf("HierarchyControl failed: %v", err)
		}
	}
	defer func() {
		hierarchyControl(tpm.PlatformHandleContext(), tpm2.HandleOwner, true)
		hierarchyControl(tpm.PlatformHandleContext(), tpm2.HandleEndorsement, true)
	}()

	hierarchyControl(tpm.OwnerHandleContext(), tpm2.HandleOwner, false)
	if tpm.IsEnabled() {
		t.Errorf("IsEnabled returned the wrong value")
	}

	hierarchyControl(tpm.EndorsementHandleContext(), tpm2.HandleEndorsement, false)
	if tpm.IsEnabled() {
		t.Errorf("IsEnabled returned the wrong value")
	}
}

func TestConnectToDefaultTPM(t *testing.T) {
	SetOpenDefaultTctiFn(func() (io.ReadWriteCloser, error) {
		return tpm2.OpenMssim("", *mssimPort, *mssimPort+1)
	})

	connectAndClear := func(t *testing.T) *TPMConnection {
		tpm, _ := openTPMSimulatorForTesting(t)
		clearTPMWithPlatformAuth(t, tpm)
		return tpm
	}

	run := func(t *testing.T, hasEk bool, cleanup func(*TPMConnection)) {
		tpm, err := ConnectToDefaultTPM()
		if err != nil {
			t.Fatalf("ConnectToDefaultTPM failed: %v", err)
		}
		defer func() {
			if cleanup != nil {
				cleanup(tpm)
			}
			closeTPM(t, tpm)
		}()

		if len(tpm.VerifiedEKCertChain()) > 0 {
			t.Errorf("Should be no verified EK cert chain")
		}
		if tpm.VerifiedDeviceAttributes() != nil {
			t.Errorf("Should be no verified device attributes")
		}
		rc, err := tpm.EndorsementKey()
		if !hasEk {
			if err == nil {
				t.Fatalf("TPMConnection.EndorsementKey should have returned an error")
			}
			if rc != nil {
				t.Errorf("TPMConnection.EndorsementKey should have returned a nil context")
			}
			if err != ErrTPMProvisioning {
				t.Errorf("TPMConnection.EndorsementKey returned an unexpected error: %v", err)
			}
		} else {
			if err != nil {
				t.Fatalf("TPMConnection.EndorsementKey failed: %v", err)
			}
			if rc == nil {
				t.Fatalf("TPMConnection.EndorsementKey returned a nil context")
			}
			if rc.Handle() != EkHandle {
				t.Errorf("TPMConnection.EndorsementKey returned an unexpected context")
			}
		}
		session := tpm.HmacSession()
		if session == nil || session.Handle().Type() != tpm2.HandleTypeHMACSession {
			t.Fatalf("TPMConnection.HmacSession returned invalid session context")
		}
	}

	t.Run("Unprovisioned", func(t *testing.T) {
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)
		}()

		run(t, false, nil)
	})

	t.Run("Provisioned", func(t *testing.T) {
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)

			if err := ProvisionTPM(tpm, ProvisionModeFull, nil); err != nil {
				t.Fatalf("ProvisionTPM failed: %v", err)
			}
		}()

		run(t, true, nil)
	})

	t.Run("InvalidEK", func(t *testing.T) {
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)

			primary, _, _, _, _, err := tpm.CreatePrimary(tpm.EndorsementHandleContext(), nil, EkTemplate, nil, nil, nil)
			if err != nil {
				t.Fatalf("CreatePrimary failed: %v", err)
			}
			defer flushContext(t, tpm, primary)

			sessionContext, err := tpm.StartAuthSession(nil, nil, tpm2.SessionTypePolicy, nil, tpm2.HashAlgorithmSHA256)
			if err != nil {
				t.Fatalf("StartAuthSession failed: %v", err)
			}
			defer flushContext(t, tpm, sessionContext)
			sessionContext.SetAttrs(tpm2.AttrContinueSession)

			if _, _, err := tpm.PolicySecret(tpm.EndorsementHandleContext(), sessionContext, nil, nil, 0, nil); err != nil {
				t.Fatalf("PolicySecret failed: %v", err)
			}

			priv, pub, _, _, _, err := tpm.Create(primary, nil, EkTemplate, nil, nil, sessionContext)
			if err != nil {
				t.Fatalf("Create failed: %v", err)
			}

			if _, _, err := tpm.PolicySecret(tpm.EndorsementHandleContext(), sessionContext, nil, nil, 0, nil); err != nil {
				t.Fatalf("PolicySecret failed: %v", err)
			}

			context, err := tpm.Load(primary, priv, pub, sessionContext)
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			defer flushContext(t, tpm, context)

			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), context, EkHandle, nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}
		}()

		run(t, false, nil)
	})
}

func TestConnectToDefaultTPMNoTPM(t *testing.T) {
	SetOpenDefaultTctiFn(func() (io.ReadWriteCloser, error) {
		return nil, &os.PathError{Op: "open", Path: "/dev/tpm0", Err: syscall.ENOENT}
	})

	tpm, err := ConnectToDefaultTPM()
	if tpm != nil {
		t.Errorf("ConnectToDefaultTPM should have failed")
	}
	if err != ErrNoTPM2Device {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestSecureConnectToDefaultTPM(t *testing.T) {
	SetOpenDefaultTctiFn(func() (io.ReadWriteCloser, error) {
		return tpm2.OpenMssim("", *mssimPort, *mssimPort+1)
	})

	connectAndClear := func(t *testing.T) *TPMConnection {
		tpm, _ := openTPMSimulatorForTesting(t)
		clearTPMWithPlatformAuth(t, tpm)
		return tpm
	}

	run := func(t *testing.T, ekCert io.Reader, hasEk bool, auth []byte, cleanup func(*TPMConnection)) {
		tpm, err := SecureConnectToDefaultTPM(ekCert, auth)
		if err != nil {
			t.Fatalf("SecureConnectToDefaultTPM failed: %v", err)
		}
		defer func() {
			if cleanup != nil {
				cleanup(tpm)
			}
			closeTPM(t, tpm)
		}()

		if len(tpm.VerifiedEKCertChain()) != 2 {
			t.Fatalf("Unexpected number of certificates in chain")
		}
		if !bytes.Equal(tpm.VerifiedEKCertChain()[0].Raw, testEkCert) {
			t.Errorf("Unexpected leaf certificate")
		}

		if tpm.VerifiedDeviceAttributes() == nil {
			t.Fatalf("Should have verified device attributes")
		}
		if tpm.VerifiedDeviceAttributes().Manufacturer != tpm2.TPMManufacturerIBM {
			t.Errorf("Unexpected verified manufacturer")
		}
		if tpm.VerifiedDeviceAttributes().Model != "FakeTPM" {
			t.Errorf("Unexpected verified model")
		}
		if tpm.VerifiedDeviceAttributes().FirmwareVersion != binary.BigEndian.Uint32([]byte{0x00, 0x01, 0x00, 0x02}) {
			t.Errorf("Unexpected verified firmware version")
		}

		rc, err := tpm.EndorsementKey()
		if !hasEk {
			if err == nil {
				t.Fatalf("TPMConnection.EndorsementKey should have returned an error")
			}
			if rc != nil {
				t.Errorf("TPMConnection.EndorsementKey should have returned a nil context")
			}
			if err != ErrTPMProvisioning {
				t.Errorf("TPMConnection.EndorsementKey returned an unexpected error: %v", err)
			}
		} else {
			if err != nil {
				t.Fatalf("TPMConnection.EndorsementKey failed: %v", err)
			}
			if rc == nil {
				t.Fatalf("TPMConnection.EndorsementKey returned a nil context")
			}
			if rc.Handle() != EkHandle {
				t.Errorf("TPMConnection.EndorsementKey returned an unexpected context")
			}
		}
		session := tpm.HmacSession()
		if session == nil || session.Handle().Type() != tpm2.HandleTypeHMACSession {
			t.Fatalf("TPMConnection.HmacSession returned invalid session context")
		}
	}

	t.Run("Unprovisioned", func(t *testing.T) {
		// Test that we verify successfully with a transient EK
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)
		}()

		run(t, bytes.NewReader(testEncodedEkCertChain), false, nil, nil)
	})

	t.Run("UnprovisionedWithEndorsementAuth", func(t *testing.T) {
		// Test that we verify successfully with a transient EK when the endorsement hierarchy has an authorization value and we know it
		testAuth := []byte("56789")
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)

			if err := tpm.HierarchyChangeAuth(tpm.EndorsementHandleContext(), testAuth, nil); err != nil {
				t.Fatalf("HierarchyChangeAuth failed: %v", err)
			}
		}()

		run(t, bytes.NewReader(testEncodedEkCertChain), false, testAuth, func(tpm *TPMConnection) {
			clearTPMWithPlatformAuth(t, tpm)
		})
	})

	t.Run("UnprovisionedWithUnknownEndorsementAuth", func(t *testing.T) {
		// Test that we get the correct error if there is no persistent EK and we can't create a transient one
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)

			if err := tpm.HierarchyChangeAuth(tpm.EndorsementHandleContext(), []byte("1234"), nil); err != nil {
				t.Fatalf("HierarchyChangeAuth failed: %v", err)
			}
		}()

		defer func() {
			tpm, err := ConnectToDefaultTPM()
			if err != nil {
				t.Fatalf("ConnectToTPM failed: %v", err)
			}
			defer closeTPM(t, tpm)
			clearTPMWithPlatformAuth(t, tpm)
		}()

		_, err := SecureConnectToDefaultTPM(bytes.NewReader(testEncodedEkCertChain), nil)
		if err == nil {
			t.Fatalf("SecureConnectToDefaultTPM should have failed")
		}
		if err != ErrTPMProvisioning {
			t.Errorf("Unexpected error: %v", err)
		}
	})

	t.Run("Provisioned", func(t *testing.T) {
		// Test that we verify successfully with the properly provisioned persistent EK
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)

			if err := ProvisionTPM(tpm, ProvisionModeFull, nil); err != nil {
				t.Fatalf("ProvisionTPM failed: %v", err)
			}
		}()

		run(t, bytes.NewReader(testEncodedEkCertChain), true, nil, nil)
	})

	t.Run("CallerProvidedEkCert", func(t *testing.T) {
		// Test that we can verify without a TPM provisioned EK certificate
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)
		}()

		cert, _ := x509.ParseCertificate(testEkCert)
		caCert, _ := x509.ParseCertificate(testCACert)

		certData := new(bytes.Buffer)
		if err := EncodeEKCertificateChain(cert, []*x509.Certificate{caCert}, certData); err != nil {
			t.Fatalf("EncodeEKCertificateChain failed: %v", err)
		}

		run(t, certData, false, nil, nil)
	})

	t.Run("InvalidEkCert", func(t *testing.T) {
		// Test that we get the right error if the provided EK cert data is invalid
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)
		}()

		certData := new(bytes.Buffer)
		certData.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00})

		_, err := SecureConnectToDefaultTPM(certData, nil)
		if err == nil {
			t.Fatalf("SecureConnectToDefaultTPM should have failed")
		}
		if _, ok := err.(EKCertVerificationError); !ok {
			t.Errorf("Unexpected error: %v", err)
		}
	})

	t.Run("EkCertUnknownIssuer", func(t *testing.T) {
		// Test that we get the right error if the provided EK cert has an unknown issuer
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)
		}()

		certData := func() io.Reader {
			caCertRaw, caKey, err := createTestCA()
			if err != nil {
				t.Fatalf("createTestCA failed: %v", err)
			}

			tpm, err := ConnectToDefaultTPM()
			if err != nil {
				t.Fatalf("ConnectToDefaultTPM failed: %v", err)
			}
			defer closeTPM(t, tpm)

			certRaw, err := createTestEkCert(tpm.TPMContext, caCertRaw, caKey)
			if err != nil {
				t.Fatalf("createTestEkCert failed: %v", err)
			}

			cert, _ := x509.ParseCertificate(certRaw)
			caCert, _ := x509.ParseCertificate(caCertRaw)

			b := new(bytes.Buffer)
			if err := EncodeEKCertificateChain(cert, []*x509.Certificate{caCert}, b); err != nil {
				t.Fatalf("EncodeEKCertificateChain failed: %v", err)
			}
			return b
		}()

		_, err := SecureConnectToDefaultTPM(certData, nil)
		if err == nil {
			t.Fatalf("SecureConnectToDefaultTPM should have failed")
		}
		if _, ok := err.(EKCertVerificationError); !ok {
			t.Errorf("Unexpected error: %v", err)
		}
	})

	t.Run("IncorrectPersistentEK", func(t *testing.T) {
		// Test that we verify successfully using a transient EK if the persistent EK doesn't match the certificate
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)

			// This produces a primary key that doesn't match the certificate created in TestMain
			sensitive := tpm2.SensitiveCreate{Data: []byte("foo")}
			ekContext, _, _, _, _, err := tpm.CreatePrimary(tpm.EndorsementHandleContext(), &sensitive, EkTemplate, nil, nil, nil)
			if err != nil {
				t.Fatalf("CreatePrimary failed: %v", err)
			}
			defer tpm.FlushContext(ekContext)
			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), ekContext, EkHandle, nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}
		}()

		run(t, bytes.NewReader(testEncodedEkCertChain), false, nil, nil)
	})

	t.Run("IncorrectPersistentEKWithEndorsementAuth", func(t *testing.T) {
		// Test that we verify successfully using a transient EK if the persistent EK doesn't match the certificate and we have set the
		// endorsement hierarchy authorization value
		testAuth := []byte("12345")
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)

			// This produces a primary key that doesn't match the certificate created in TestMain
			sensitive := tpm2.SensitiveCreate{Data: []byte("foo")}
			ekContext, _, _, _, _, err := tpm.CreatePrimary(tpm.EndorsementHandleContext(), &sensitive, EkTemplate, nil, nil, nil)
			if err != nil {
				t.Fatalf("CreatePrimary failed: %v", err)
			}
			defer tpm.FlushContext(ekContext)
			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), ekContext, EkHandle, nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}

			if err := tpm.HierarchyChangeAuth(tpm.EndorsementHandleContext(), testAuth, nil); err != nil {
				t.Fatalf("HierarchyChangeAuth failed: %v", err)
			}
		}()

		run(t, bytes.NewReader(testEncodedEkCertChain), false, testAuth, func(tpm *TPMConnection) {
			clearTPMWithPlatformAuth(t, tpm)
		})
	})

	t.Run("IncorrectPersistentEKWithUnknownEndorsementAuth", func(t *testing.T) {
		// Verify that we get the expected error if the persistent EK doesn't match the certificate and we can't create a transient EK
		testAuth := []byte("12345")
		func() {
			tpm := connectAndClear(t)
			defer closeTPM(t, tpm)

			// This produces a primary key that doesn't match the certificate created in TestMain
			sensitive := tpm2.SensitiveCreate{Data: []byte("foo")}
			ekContext, _, _, _, _, err := tpm.CreatePrimary(tpm.EndorsementHandleContext(), &sensitive, EkTemplate, nil, nil, nil)
			if err != nil {
				t.Fatalf("CreatePrimary failed: %v", err)
			}
			defer tpm.FlushContext(ekContext)
			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), ekContext, EkHandle, nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}

			if err := tpm.HierarchyChangeAuth(tpm.EndorsementHandleContext(), testAuth, nil); err != nil {
				t.Fatalf("HierarchyChangeAuth failed: %v", err)
			}
		}()

		defer func() {
			tpm, err := ConnectToDefaultTPM()
			if err != nil {
				t.Fatalf("ConnectToTPM failed: %v", err)
			}
			defer closeTPM(t, tpm)
			clearTPMWithPlatformAuth(t, tpm)
		}()

		_, err := SecureConnectToDefaultTPM(bytes.NewReader(testEncodedEkCertChain), nil)
		if err == nil {
			t.Fatalf("SecureConnectToDefaultTPM should have failed")
		}
		if _, ok := err.(TPMVerificationError); !ok {
			t.Errorf("Unexpected error: %v", err)
		}
	})
}

func TestMain(m *testing.M) {
	flag.Parse()
	rand.Seed(time.Now().UnixNano())
	os.Exit(func() int {
		if *useMssim {
			mssimPath := ""
			for _, p := range []string{"tpm2-simulator", "tpm2-simulator-chrisccoulson.tpm2-simulator"} {
				var err error
				mssimPath, err = exec.LookPath(p)
				if err == nil {
					break
				}
			}
			if mssimPath == "" {
				fmt.Fprintf(os.Stderr, "Cannot find a TPM simulator binary\n")
				return 1
			}

			// The TPM simulator creates its persistent storage in its current directory. Ideally, we would create
			// a unique temporary directory for it, but this doesn't work with the snap because it has its own private
			// tmpdir. Detect whether the chosen TPM simulator is a snap, determine which snap it belongs to and create
			// a temporary directory inside its common data directory instead.
			mssimSnapName := ""
			for currentPath, lastPath := mssimPath, ""; currentPath != ""; {
				dest, err := os.Readlink(currentPath)
				switch {
				case err != nil:
					if filepath.Base(currentPath) == "snap" {
						mssimSnapName, _ = snap.SplitSnapApp(filepath.Base(lastPath))
					}
					currentPath = ""
				default:
					if !filepath.IsAbs(dest) {
						dest = filepath.Join(filepath.Dir(currentPath), dest)
					}
					lastPath = currentPath
					currentPath = dest
				}
			}

			tmpRoot := ""
			if mssimSnapName != "" {
				home := os.Getenv("HOME")
				if home == "" {
					fmt.Fprintf(os.Stderr, "Cannot determine home directory\n")
					return 1
				}
				tmpRoot = snap.UserCommonDataDir(home, mssimSnapName)
				if err := os.MkdirAll(tmpRoot, 0755); err != nil {
					fmt.Fprintf(os.Stderr, "Cannot create snap common data dir: %v\n", err)
					return 1
				}
			}

			mssimTmpDir, err := ioutil.TempDir(tmpRoot, "secboot.mssim")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Cannot create temporary directory for TPM simulator: %v\n", err)
				return 1
			}
			defer os.RemoveAll(mssimTmpDir)

			cmd := exec.Command(mssimPath, "-m", strconv.FormatUint(uint64(*mssimPort), 10))
			cmd.Dir = mssimTmpDir
			// The tpm2-simulator-chrisccoulson snap originally had a patch to chdir in to the root of the snap's common data directory,
			// where it would store its persistent data. We don't want this behaviour now. This environment variable exists until all
			// secboot and go-tpm2 branches have been fixed to not depend on this behaviour.
			cmd.Env = append(cmd.Env, "TPM2SIM_DONT_CD_TO_HOME=1")

			if err := cmd.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "Cannot start TPM simulator: %v\n", err)
				return 1
			}

			if cert, key, err := createTestCA(); err != nil {
				fmt.Fprintf(os.Stderr, "Cannot create test TPM CA certificate and private key: %v\n", err)
				return 1
			} else {
				h := crypto.SHA256.New()
				h.Write(cert)
				AppendRootCAHash(h.Sum(nil))

				testCACert = cert
				testCAKey = key
			}

			var tcti *tpm2.TctiMssim
			// Give the simulator 5 seconds to start up
		Loop:
			for i := 0; ; i++ {
				var err error
				tcti, err = tpm2.OpenMssim("", *mssimPort, *mssimPort+1)
				switch {
				case err != nil && i == 4:
					fmt.Fprintf(os.Stderr, "Cannot open TPM simulator connection: %v", err)
					return 1
				case err != nil:
					time.Sleep(time.Second)
				default:
					break Loop
				}
			}

			tpm, _ := tpm2.NewTPMContext(tcti)

			if err := func() error {
				defer tpm.Close()

				if err := tpm.Startup(tpm2.StartupClear); err != nil {
					return err
				}

				return certifyTPM(tpm)
			}(); err != nil {
				fmt.Fprintf(os.Stderr, "TPM simulator startup failed: %v\n", err)
				return 1
			}

			defer func() {
				if cmd == nil || cmd.Process == nil {
					return
				}
				cleanShutdown := false
				defer func() {
					if cleanShutdown {
						if err := cmd.Wait(); err != nil {
							fmt.Fprintf(os.Stderr, "TPM simulator finished with an error: %v", err)
						}
					} else {
						fmt.Fprintf(os.Stderr, "Killing TPM simulator\n")
						if err := cmd.Process.Kill(); err != nil {
							fmt.Fprintf(os.Stderr, "Cannot send signal to TPM simulator: %v\n", err)
						}
					}
				}()

				tcti, err := tpm2.OpenMssim("", *mssimPort, *mssimPort+1)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Cannot open TPM simulator connection for shutdown: %v\n", err)
					return
				}

				tpm, _ := tpm2.NewTPMContext(tcti)
				if err := tpm.Shutdown(tpm2.StartupClear); err != nil {
					fmt.Fprintf(os.Stderr, "TPM simulator shutdown failed: %v\n", err)
				}
				if err := tcti.Stop(); err != nil {
					fmt.Fprintf(os.Stderr, "TPM simulator stop failed: %v\n", err)
					return
				}
				if err := tpm.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "TPM simulator connection close failed: %v\n", err)
					return
				}
				cleanShutdown = true
			}()
		}

		return m.Run()
	}())
}
