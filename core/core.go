/*
Copyright © 2021-2022 Manetu Inc. All Rights Reserved.
*/

package core

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/ThalesIgnite/crypto11"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/viper"

	"github.com/manetu/security-token/config"
)

// Check panics if err != nil
func Check(e error) {
	if e != nil {
		panic(e)
	}
}

func randomID() ([]byte, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}

	return token, nil
}

// HexEncode encodes the raw bytes into hex
func HexEncode(b []byte) string {
	var buf bytes.Buffer
	for i, f := range b {
		if i > 0 {
			_, _ = fmt.Fprintf(&buf, ":")
		}
		_, _ = fmt.Fprintf(&buf, "%02X", f)
	}

	return buf.String()
}

func importHexencode(serial string) []byte {
	reg, err := regexp.Compile(":")
	Check(err)

	striped := reg.ReplaceAllString(serial, "")
	b, err := hex.DecodeString(striped)
	Check(err)

	return b
}

// ExportCert exports the certificate into a PEM string
func ExportCert(cert *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

type Core struct {
	ctx     *crypto11.Context
	Backend config.BackendConfiguration
}

func New() Core {
	viper.SetConfigName("security-tokens")
	viper.AddConfigPath(".")
	viper.AddConfigPath("$HOME/.manetu")
	viper.AddConfigPath("/etc/manetu/")
	var configuration config.Configuration

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Error reading config file, %s", err)
	}
	err := viper.Unmarshal(&configuration)
	if err != nil {
		log.Fatalf("unable to decode into struct, %v", err)
	}

	// Configure PKCS#11 library via configuration file
	ctx, err := crypto11.Configure(&crypto11.Config{
		Path:       configuration.Pkcs11.Path,
		TokenLabel: configuration.Pkcs11.TokenLabel,
		Pin:        configuration.Pkcs11.Pin,
	})
	Check(err)

	fmt.Fprintf(os.Stderr, "Using config file: %s\n", viper.ConfigFileUsed())

	return Core{ctx: ctx, Backend: configuration.Backend}
}

func (c Core) Close() error {
	return c.ctx.Close()
}

type Token struct {
	Signer crypto11.Signer
	Cert   *x509.Certificate
}

func (c Core) getToken(serial string) (*Token, error) {

	var id []byte

	if serial == "" {
		certs, err := c.ctx.FindAllPairedCertificates()
		if err != nil {
			return nil, err
		}

		if len(certs) < 1 {
			return nil, errors.New("no security-tokens found")
		}

		id = certs[0].Leaf.SerialNumber.Bytes()
	} else {
		id = importHexencode(serial)
	}

	signer, err := c.ctx.FindKeyPair(id, nil)
	if err != nil {
		return nil, err
	}
	if signer == nil {
		return nil, errors.New("invalid serial number")
	}

	cert, err := c.ctx.FindCertificate(id, nil, nil)
	if err != nil {
		return nil, err
	}
	if cert == nil {
		return nil, errors.New("certificate not found")
	}

	return &Token{
		Signer: signer,
		Cert:   cert,
	}, nil
}

func (c Core) Show(serial string) {
	token, err := c.getToken(serial)
	Check(err)

	fmt.Printf("%s\n", ExportCert(token.Cert))
}

func (c Core) List() {
	certs, err := c.ctx.FindAllPairedCertificates()
	Check(err)

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Serial", "Provider", "Created"})

	for _, x := range certs {
		cert := x.Leaf
		// there may multiple providers in future ?
		providers := cert.Subject.Organization[0]
		for i := 1; i < len(cert.Subject.Organization); i++ {
			providers = "," + cert.Subject.Organization[i]
		}
		table.Append([]string{HexEncode(cert.SerialNumber.Bytes()), providers, cert.NotBefore.String()})
	}
	table.Render() // Send output
}

// ComputeMRN computes MRN given certificate
func ComputeMRN(cert *x509.Certificate) string {
	hash := sha256.Sum256(cert.Raw)
	return "mrn:iam:" + cert.Subject.Organization[0] + ":identity:" + hex.EncodeToString(hash[:])
}

func (c Core) Generate(provider string) (*x509.Certificate, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}

	signer, err := c.ctx.GenerateECDSAKeyPair(id, elliptic.P256())
	if err != nil {
		return nil, err
	}

	now := time.Now()
	duration := time.Hour * 24 * 3650
	template := x509.Certificate{
		SerialNumber: new(big.Int).SetBytes(id),
		Subject: pkix.Name{
			Organization: []string{provider},
			SerialNumber: HexEncode(id),
		},
		NotBefore:             now,
		NotAfter:              now.Add(duration),
		BasicConstraintsValid: true,
		IsCA:                  false,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, signer.Public(), signer)
	if err != nil {
		return nil, err
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	cp := x509.NewCertPool()
	cp.AddCert(cert)

	_, err = cert.Verify(x509.VerifyOptions{
		Roots: cp,
	})
	if err != nil {
		return nil, err
	}

	err = c.ctx.ImportCertificate(id, cert)
	if err != nil {
		return nil, err
	}

	return cert, nil
}

func (c Core) Delete(serial string) error {
	id := importHexencode(serial)

	err := c.ctx.DeleteCertificate(id, nil, nil)
	if err != nil {
		return err
	}

	signer, err := c.ctx.FindKeyPair(id, nil)
	if err != nil {
		return err
	}

	if signer == nil {
		_, _ = fmt.Fprint(os.Stderr, "ERROR: Invalid serial number")
		return nil
	}

	return signer.Delete()
}

func (c Core) Login(signer crypto.Signer, cert *x509.Certificate) (string, error) {
	mrn := ComputeMRN(cert)
	cajwt, err := createJWT(signer, mrn, c.Backend.TokenURL)
	if err != nil {
		return "", err
	}

	jwt, err := login(cajwt, mrn, c.Backend.TokenURL)
	if err != nil {
		return "", err
	}

	return jwt, err
}

func (c Core) LoginPKCS11(serial string) (string, error) {
	token, err := c.getToken(serial)
	if err != nil {
		return "", err
	}

	return c.Login(token.Signer, token.Cert)
}

func (c Core) pathToBytes(path string) ([]byte, error) {
	return os.ReadFile(filepath.Clean(path))
}

func (c Core) LoginX509(key string, cert string, path bool) (string, error) {
	var (
		kBytes []byte
		cBytes []byte
		err    error
	)

	if path {
		kBytes, err = c.pathToBytes(key)
		if err != nil {
			return "", err
		}
		cBytes, err = c.pathToBytes(cert)
		if err != nil {
			return "", err
		}
	} else {
		kBytes = []byte(key)
		cBytes = []byte(cert)
	}

	getSigner := func(key []byte) (crypto.Signer, error) {
		block, _ := pem.Decode(key)
		a, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}

		var (
			signer crypto.Signer
			ok     bool
		)

		if signer, ok = a.(*ecdsa.PrivateKey); !ok {
			return nil, fmt.Errorf("unsupported private key")
		}

		return signer, nil
	}

	signer, err := getSigner(kBytes)
	if err != nil {
		return "", err
	}

	certB, _ := pem.Decode(cBytes)
	xCert, err := x509.ParseCertificate(certB.Bytes)
	if err != nil {
		return "", err
	}

	return c.Login(signer, xCert)
}
