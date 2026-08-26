package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyReleaseSignature(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	directory := t.TempDir()
	checksumsPath := filepath.Join(directory, "checksums.txt")
	signaturePath := filepath.Join(directory, "checksums.txt.sig")
	checksums := []byte("abc  ecs-controller-linux-amd64.tar.gz\n")
	if err := os.WriteFile(checksumsPath, checksums, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signaturePath, ed25519.Sign(privateKey, checksums), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseSignature(publicPEM, checksumsPath, signaturePath); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := os.WriteFile(checksumsPath, append(checksums, 'x'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseSignature(publicPEM, checksumsPath, signaturePath); err == nil {
		t.Fatal("tampered checksums must be rejected")
	}
}
