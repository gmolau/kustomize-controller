/*
Copyright 2022 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package awskms

import (
	"context"
	"encoding/base64"
	"fmt"
	logger "log"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	. "github.com/onsi/gomega"
	"github.com/ory/dockertest"
)

var (
	testKMSServerURL string
	testKMSARN       string
)

const (
	dummyARN          = "arn:aws:kms:us-west-2:107501996527:key/612d5f0p-p1l3-45e6-aca6-a5b005693a48"
	testLocalKMSTag   = "3.11.1"
	testLocalKMSImage = "nsmithuk/local-kms"
)

// TestMain initializes a AWS KMS server using Docker, writes the HTTP address to
// testAWSEndpoint, tries to generate a key for encryption-decryption using a
// backoff retry approach and then sets testKMSARN to the id of the generated key.
// It then runs all the tests, which can make use of the various `test*` variables.
func TestMain(m *testing.M) {
	// Uses a sensible default on Windows (TCP/HTTP) and Linux/MacOS (socket)
	pool, err := dockertest.NewPool("")
	if err != nil {
		logger.Fatalf("could not connect to docker: %s", err)
	}

	// Pull the image, create a container based on it, and run it
	// resource, err := pool.Run("nsmithuk/local-kms", testLocalKMSVersion, []string{})
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository:   testLocalKMSImage,
		Tag:          testLocalKMSTag,
		ExposedPorts: []string{"8080"},
	})
	if err != nil {
		logger.Fatalf("could not start resource: %s", err)
	}

	purgeResource := func() {
		if err := pool.Purge(resource); err != nil {
			logger.Printf("could not purge resource: %s", err)
		}
	}

	testKMSServerURL = fmt.Sprintf("http://127.0.0.1:%v", resource.GetPort("8080/tcp"))
	masterKey := createTestMasterKey(dummyARN)

	kmsClient, err := createTestKMSClient(masterKey)
	if err != nil {
		purgeResource()
		logger.Fatalf("could not create session: %s", err)
	}

	var key *kms.CreateKeyOutput
	if err := pool.Retry(func() error {
		key, err = kmsClient.CreateKey(context.TODO(), &kms.CreateKeyInput{})
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		purgeResource()
		logger.Fatalf("could not create key: %s", err)
	}

	if key.KeyMetadata.Arn != nil {
		testKMSARN = *key.KeyMetadata.Arn
	} else {
		purgeResource()
		logger.Fatalf("could not set arn")
	}

	// Run the tests, but only if we succeeded in setting up the AWS KMS server.
	var code int
	if err == nil {
		code = m.Run()
	}

	// This can't be deferred, as os.Exit simpy does not care
	if err := pool.Purge(resource); err != nil {
		logger.Fatalf("could not purge resource: %s", err)
	}

	os.Exit(code)
}

func TestMasterKey_Encrypt(t *testing.T) {
	g := NewWithT(t)
	key := createTestMasterKey(testKMSARN)
	dataKey := []byte("thisistheway")
	g.Expect(key.Encrypt(dataKey)).To(Succeed())
	g.Expect(key.EncryptedKey).ToNot(BeEmpty())

	kmsClient, err := createTestKMSClient(key)
	g.Expect(err).ToNot(HaveOccurred())

	k, err := base64.StdEncoding.DecodeString(key.EncryptedKey)
	g.Expect(err).ToNot(HaveOccurred())

	input := &kms.DecryptInput{
		CiphertextBlob:    k,
		EncryptionContext: key.EncryptionContext,
	}
	decrypted, err := kmsClient.Decrypt(context.TODO(), input)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(decrypted.Plaintext).To(Equal(dataKey))
}

func TestMasterKey_Encrypt_SOPS_Compat(t *testing.T) {
	g := NewWithT(t)

	encryptKey := createTestMasterKey(testKMSARN)
	dataKey := []byte("encrypt-compat")
	g.Expect(encryptKey.Encrypt(dataKey)).To(Succeed())

	decryptKey := createTestMasterKey(testKMSARN)
	decryptKey.credentialsProvider = nil
	decryptKey.EncryptedKey = encryptKey.EncryptedKey
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	dec, err := decryptKey.Decrypt()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dec).To(Equal(dataKey))
}

func TestMasterKey_EncryptIfNeeded(t *testing.T) {
	g := NewWithT(t)

	key := createTestMasterKey(testKMSARN)
	g.Expect(key.EncryptIfNeeded([]byte("data"))).To(Succeed())

	encryptedKey := key.EncryptedKey
	g.Expect(encryptedKey).ToNot(BeEmpty())

	g.Expect(key.EncryptIfNeeded([]byte("some other data"))).To(Succeed())
	g.Expect(key.EncryptedKey).To(Equal(encryptedKey))
}

func TestMasterKey_EncryptedDataKey(t *testing.T) {
	g := NewWithT(t)

	key := &MasterKey{EncryptedKey: "some key"}
	g.Expect(key.EncryptedDataKey()).To(BeEquivalentTo(key.EncryptedKey))
}

func TestMasterKey_Decrypt(t *testing.T) {
	g := NewWithT(t)

	key := createTestMasterKey(testKMSARN)
	kmsClient, err := createTestKMSClient(key)
	g.Expect(err).ToNot(HaveOccurred())

	dataKey := []byte("itsalwaysdns")
	out, err := kmsClient.Encrypt(context.TODO(), &kms.EncryptInput{
		Plaintext: dataKey, KeyId: &key.Arn, EncryptionContext: key.EncryptionContext,
	})
	g.Expect(err).ToNot(HaveOccurred())

	key.EncryptedKey = base64.StdEncoding.EncodeToString(out.CiphertextBlob)
	got, err := key.Decrypt()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(dataKey))
}

func TestMasterKey_Decrypt_SOPS_Compat(t *testing.T) {
	g := NewWithT(t)

	dataKey := []byte("decrypt-compat")

	encryptKey := createTestMasterKey(testKMSARN)
	encryptKey.credentialsProvider = nil
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	g.Expect(encryptKey.Encrypt(dataKey)).To(Succeed())

	decryptKey := createTestMasterKey(testKMSARN)
	decryptKey.EncryptedKey = encryptKey.EncryptedKey
	dec, err := decryptKey.Decrypt()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dec).To(Equal(dataKey))
}

func TestMasterKey_EncryptDecrypt_RoundTrip(t *testing.T) {
	g := NewWithT(t)

	dataKey := []byte("thisistheway")

	encryptKey := createTestMasterKey(testKMSARN)
	g.Expect(encryptKey.Encrypt(dataKey)).To(Succeed())
	g.Expect(encryptKey.EncryptedKey).ToNot(BeEmpty())

	decryptKey := createTestMasterKey(testKMSARN)
	decryptKey.EncryptedKey = encryptKey.EncryptedKey

	decryptedData, err := decryptKey.Decrypt()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(decryptedData).To(Equal(dataKey))
}

func TestMasterKey_NeedsRotation(t *testing.T) {
	g := NewWithT(t)

	key := NewMasterKeyFromArn(dummyARN, nil, "")
	g.Expect(key.NeedsRotation()).To(BeFalse())

	key.CreationDate = key.CreationDate.Add(-(kmsTTL + time.Second))
	g.Expect(key.NeedsRotation()).To(BeTrue())
}

func TestMasterKey_ToMap(t *testing.T) {
	g := NewWithT(t)
	key := MasterKey{
		Arn:          "test-arn",
		Role:         "test-role",
		EncryptedKey: "enc-key",
		EncryptionContext: map[string]string{
			"env": "test",
		},
	}
	g.Expect(key.ToMap()).To(Equal(map[string]interface{}{
		"arn":        "test-arn",
		"role":       "test-role",
		"created_at": "0001-01-01T00:00:00Z",
		"enc":        "enc-key",
		"context": map[string]string{
			"env": "test",
		},
	}))
}

func TestCreds_ApplyToMasterKey(t *testing.T) {
	g := NewWithT(t)

	creds := CredsProvider{
		credsProvider: credentials.NewStaticCredentialsProvider("", "", ""),
	}
	key := &MasterKey{}
	creds.ApplyToMasterKey(key)
	g.Expect(key.credentialsProvider).To(Equal(creds.credsProvider))
}

func TestLoadAwsKmsCredsFromYaml(t *testing.T) {
	g := NewWithT(t)
	credsYaml := []byte(`
aws_access_key_id: test-id
aws_secret_access_key: test-secret
aws_session_token: test-token
`)
	credsProvider, err := LoadCredsProviderFromYaml(credsYaml)
	g.Expect(err).ToNot(HaveOccurred())

	creds, err := credsProvider.credsProvider.Retrieve(context.TODO())
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(creds.AccessKeyID).To(Equal("test-id"))
	g.Expect(creds.SecretAccessKey).To(Equal("test-secret"))
	g.Expect(creds.SessionToken).To(Equal("test-token"))
}

func Test_createKMSConfig(t *testing.T) {
	g := NewWithT(t)

	key := MasterKey{
		credentialsProvider: credentials.NewStaticCredentialsProvider("test-id", "test-secret", "test-token"),
	}
	cfg, err := key.createKMSConfig()
	g.Expect(err).ToNot(HaveOccurred())

	creds, err := cfg.Credentials.Retrieve(context.TODO())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(creds.AccessKeyID).To(Equal("test-id"))
	g.Expect(creds.SecretAccessKey).To(Equal("test-secret"))
	g.Expect(creds.SessionToken).To(Equal("test-token"))

	// test if we fallback to the default way of fetching credentials
	// if no static credentials are provided.
	key.credentialsProvider = nil
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "token")

	cfg, err = key.createKMSConfig()
	g.Expect(err).ToNot(HaveOccurred())

	creds, err = cfg.Credentials.Retrieve(context.TODO())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(creds.AccessKeyID).To(Equal("id"))
	g.Expect(creds.SecretAccessKey).To(Equal("secret"))
	g.Expect(creds.SessionToken).To(Equal("token"))
}

func createTestMasterKey(arn string) MasterKey {
	return MasterKey{
		Arn:                 arn,
		credentialsProvider: credentials.NewStaticCredentialsProvider("id", "secret", ""),
		epResolver:          epResolver{},
	}
}

// epResolver is a dummy resolver that points to the local test KMS server
type epResolver struct{}

func (e epResolver) ResolveEndpoint(service, region string) (aws.Endpoint, error) {
	return aws.Endpoint{
		URL: testKMSServerURL,
	}, nil
}

func createTestKMSClient(key MasterKey) (*kms.Client, error) {
	cfg, err := key.createKMSConfig()
	if err != nil {
		return nil, err
	}

	cfg.EndpointResolver = epResolver{}

	return kms.NewFromConfig(*cfg), nil
}
