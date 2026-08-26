package app

import (
	"crypto/ed25519"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"fmt"
	"os"
)

//go:embed release-public-key.pem
var releasePublicKeyPEM []byte

func VerifyReleaseSignature(checksumsPath, signaturePath string) error {
	return verifyReleaseSignature(releasePublicKeyPEM, checksumsPath, signaturePath)
}

func verifyReleaseSignature(publicKeyPEM []byte, checksumsPath, signaturePath string) error {
	checksums, err := readReleaseFile(checksumsPath, 4<<20)
	if err != nil {
		return fmt.Errorf("读取校验文件: %w", err)
	}
	signature, err := readReleaseFile(signaturePath, ed25519.SignatureSize)
	if err != nil {
		return fmt.Errorf("读取签名文件: %w", err)
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("签名长度无效")
	}
	block, _ := pem.Decode(publicKeyPEM)
	if block == nil {
		return fmt.Errorf("内置发布公钥无效")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("解析发布公钥: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("发布公钥类型无效")
	}
	if !ed25519.Verify(publicKey, checksums, signature) {
		return fmt.Errorf("Ed25519 签名校验失败")
	}
	return nil
}

func readReleaseFile(path string, maxSize int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxSize {
		return nil, fmt.Errorf("文件大小无效")
	}
	return os.ReadFile(path)
}
