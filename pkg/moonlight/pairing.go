package moonlight

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1_apply "games-on-whales.github.io/direwolf/pkg/generated/applyconfiguration/api/v1alpha1"
	v1alpha1_client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/util"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
)

func isDebuggerAttached() bool {
	// _, debug := os.LookupEnv("GODEBUG")
	// return debug
	return true
}

type PairingResponse struct {
	Response          `xml:",inline"`
	Paired            int    `xml:"paired"`
	PlainCert         string `xml:"plaincert,omitempty"`
	ChallengeResponse string `xml:"challengeresponse,omitempty"`
	PairingSecret     string `xml:"pairingsecret,omitempty"`
}

// pendingPairCacheEntry holds pairing state per client
type pendingPairCacheEntry struct {
	ClientCert      *x509.Certificate
	AESKey          []byte
	LastPhase       string
	ServerSecret    []byte
	ServerChallenge []byte
	ClientHash      []byte
	Username        string
}

type PairingManager struct {
	// Stores currently pending pairing sessions that have not yet completed.
	// They are at various stages of the pairing process.
	// key: clientID@clientIP
	// value: pendingPairCacheEntry
	PairingCache sync.Map // Go's equivalent of an immer::map for thread safety

	// Map of pairing secret to <-chan string for pin code where a request is
	// pending
	// key: randomly generated ephemeral secret for /pair request
	// value: channel to send pin code
	PendingPins sync.Map

	// Certificate of the secure serving endpoint used for pairing
	SecureServerCerificate tls.Certificate

	PairingsClient v1alpha1_client.PairingInterface
}

func NewPairingManager(
	cert tls.Certificate,
	pairingsClient v1alpha1_client.PairingInterface,
) *PairingManager {
	return &PairingManager{
		SecureServerCerificate: cert,
		PairingsClient:         pairingsClient,
	}
}

func failPair(statusMsg string) PairingResponse {
	log.Printf("Failed pairing: %s", statusMsg)
	return PairingResponse{Paired: 0, Response: Response{StatusCode: 400, StatusMessage: statusMsg}}
}

func (m *PairingManager) PostPin(secret string, pin string) error {
	channel, found := m.PendingPins.Load(secret)
	if !found {
		err := fmt.Errorf("no pending pin for secret %s", secret)
		return err
	}

	select {
	case channel.(chan string) <- pin:
		log.Printf("Sent pin %s to channel for secret %s", pin, secret)
		return nil
	default:
		err := fmt.Errorf("failed to send pin %s to channel for secret %s. Either full buffer or closed channel", pin, secret)
		return err
	}
}

func (m *PairingManager) Unpair(cacheKey string) error {
	//!TODO: Persist unpaired clients
	m.PairingCache.Delete(cacheKey)
	return nil
}

/**
 * @brief Pair, phase 1:
 *
 * Moonlight will send a salt and client certificate, we'll also need the user provided pin.
 *
 * PIN and SALT will be used to derive a shared AES key that needs to be stored
 * in order to be used to decrypt_symmetric in the next phases (see `gen_aes_key`).
 *
 * At this stage we only have to send back our public certificate (`plaincert`).
 */
func (m *PairingManager) pairPhase1(cacheKey string, salt string, clientCertStr string) PairingResponse {
	// Check if pairing session exists
	if _, found := m.PairingCache.Load(cacheKey); found {
		m.PairingCache.Delete(cacheKey)
		return failPair("Out of order pair request (phase 1)")
	}

	clientCertData, err := hex.DecodeString(clientCertStr)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to decode client cert: %s", err))
	}

	clientCertDER, _ := pem.Decode(clientCertData)
	clientCert, err := x509.ParseCertificate(clientCertDER.Bytes)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to parse client cert: %s", err))
	}

	saltData, err := hex.DecodeString(salt)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to decode salt: %s", err))
	}

	// Create a new pairing session
	pinSecret, err := util.SecureRandomBytes(8)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to generate pin secret: %s", err))
	}
	pinSecretHex := hex.EncodeToString(pinSecret)

	// Store the pin secret
	pinChannel := make(chan string, 1)
	defer close(pinChannel)
	defer m.PendingPins.Delete(pinSecretHex)
	m.PendingPins.Store(pinSecretHex, pinChannel)

	//!TODO: Get proper hostname
	log.Printf("Insert pin at http://%s/pin/#%s", "127.0.0.1:47989", pinSecretHex)

	// Hardcoded pin for testing in debug builds if debugger is attached
	var pin string
	if isDebuggerAttached() {
		log.Printf("Debugger attached, using hardcoded pin")
		pin = "1111"
	} else {
		pin = <-pinChannel
	}

	// Generate server cert and AES key
	// Store the pairing session
	m.PairingCache.Store(cacheKey, pendingPairCacheEntry{
		ClientCert: clientCert,
		LastPhase:  "GETSERVERCERT",
		Username:   "alex", // TODO: TEMPORARY: We should serve the PIN auth page under authenticated SSL to get username
		AESKey:     util.Hash(saltData[:16], []byte(pin))[:16],
	})

	// Send hex encoded server cert pem
	return PairingResponse{
		Paired: 1,
		//!TODO: Dont keep calculating this PEM every time.
		PlainCert: hex.EncodeToString([]byte(util.CertificateChainToPEM(m.SecureServerCerificate))),
		Response:  Response{StatusCode: 200},
	}
}

/**
 * @brief Pair, phase 2
 *
 * Using the AES key that we generated in the phase 1 we have to decrypt the client challenge,
 *
 * We generate a SHA256 hash with the following:
 *  - Decrypted challenge
 *  - Server certificate signature
 *  - Server secret: a randomly generated secret
 *
 * The hash + server_challenge will then be AES encrypted and sent as the `challengeresponse` in the returned XML
 */
func (m *PairingManager) pairPhase2(cacheKey string, clientChallenge string) PairingResponse {
	clientChallengeBytes, err := hex.DecodeString(clientChallenge)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to decode client challenge: %s", err))
	}

	val, found := m.PairingCache.Load(cacheKey)
	if !found {
		return failPair("Pairing session not found")
	}

	clientCache := val.(pendingPairCacheEntry)
	if clientCache.LastPhase != "GETSERVERCERT" {
		return failPair("Out of order pair request (phase 2)")
	}

	// Gernerate random server secret and challenge
	serverSecret, err := util.SecureRandomBytes(16)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to generate server secret: %s", err))
	}
	serverChallenge, err := util.SecureRandomBytes(16)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to generate server challenge: %s", err))
	}

	//!TODO: Precompute/Grab of x509 somewhere. This sucks
	serverCertSignature, err := util.ExtractCertSignature(m.SecureServerCerificate.Certificate[0])
	if err != nil {
		return failPair(fmt.Sprintf("Failed to extract server cert signature: %s", err))
	}

	decryptedChallenge, err := aesDecryptECB(clientChallengeBytes, clientCache.AESKey)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to decrypt client challenge: %s", err))
	}

	plainText := append(util.Hash(decryptedChallenge, serverCertSignature, serverSecret), serverChallenge...)
	encryptedChallenge, err := aesEncryptECB(plainText, clientCache.AESKey)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to encrypt server challenge: %s", err))
	}

	clientCache.LastPhase = "CLIENTCHALLENGE"
	clientCache.ServerSecret = serverSecret
	clientCache.ServerChallenge = serverChallenge
	m.PairingCache.Store(cacheKey, clientCache)

	return PairingResponse{
		Paired:            1,
		ChallengeResponse: hex.EncodeToString(encryptedChallenge),
		Response:          Response{StatusCode: 200},
	}
}

/**
 * @brief Pair, phase 3
 *
 * Moonlight will send back a `serverchallengeresp`: an AES encrypted client hash,
 * we have to send back the `pairingsecret`:
 * using our private key we have to sign the certificate_signature + server_secret (generated in phase 2)
 *
 * @return pair<ptree, string> the response and the decrypted client_hash
 */
func (m *PairingManager) pairPhase3(cacheKey string, serverChallenge string) PairingResponse {
	serverChallengeBytes, err := hex.DecodeString(serverChallenge)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to decode server challenge: %s", err))
	}

	val, found := m.PairingCache.Load(cacheKey)
	if !found {
		return failPair("Pairing session not found")
	}

	clientCache := val.(pendingPairCacheEntry)
	if clientCache.LastPhase != "CLIENTCHALLENGE" {
		return failPair("Out of order pair request (phase 3)")
	}

	decryptedChallenge, err := aesDecryptECB(serverChallengeBytes, clientCache.AESKey)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to decrypt server challenge: %s", err))
	}

	signature, err := m.SecureServerCerificate.PrivateKey.(crypto.Signer).Sign(rand.Reader, util.Hash(clientCache.ServerSecret), crypto.SHA256)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to sign server secret: %s", err))
	}

	pairingSecret := hex.EncodeToString(bytes.Join([][]byte{clientCache.ServerSecret, signature}, nil))

	clientCache.LastPhase = "SERVERCHALLENGERESP"
	clientCache.ClientHash = decryptedChallenge

	m.PairingCache.Store(cacheKey, clientCache)
	return PairingResponse{
		Paired:        1,
		PairingSecret: pairingSecret,
		Response:      Response{StatusCode: 200},
	}
}

/**
 * @brief Pair, phase 4 (final)
 *
 * We now have to use everything we exchanged before in order to verify and finally pair the clients
 *
 * We'll check the client_hash obtained at phase 3, it should contain the following:
 *   - The original server_challenge
 *   - The signature of the X509 client_cert
 *   - The unencrypted client_pairing_secret
 * We'll check that SHA256(server_challenge + client_public_cert_signature + client_pairing_secret) == client_hash
 *
 * Then using the client certificate public key we should be able to verify that
 * the client secret has been signed by Moonlight
 *
 * @return ptree: The XML response will contain:
 * paired = 1, if all checks are fine
 * paired = 0, otherwise
 */
func (m *PairingManager) pairPhase4(cacheKey string, pairingSecret string) PairingResponse {
	pairingSecretData, err := hex.DecodeString(pairingSecret)
	if err != nil {
		return failPair(fmt.Sprintf("Failed to decode pairing secret: %s", err))
	} else if len(pairingSecretData) < 256 {
		return failPair("Invalid pairing secret")
	}

	val, found := m.PairingCache.Load(cacheKey)
	if !found {
		return failPair("Pairing session not found")
	}

	clientCache := val.(pendingPairCacheEntry)
	if clientCache.LastPhase != "SERVERCHALLENGERESP" {
		return failPair("Out of order pair request (phase 4)")
	}

	clientSecret := pairingSecretData[:16]
	clientSignature := pairingSecretData[16:]

	if !bytes.Equal(util.Hash(
		clientCache.ServerChallenge,
		clientCache.ClientCert.Signature,
		clientSecret,
	), clientCache.ClientHash) {
		return failPair("Client hash mismatch")
	} else if clientCache.ClientCert == nil {
		return failPair("Client cert not found")
	} else if err := util.VerifySignature(clientCache.ClientCert.PublicKey, clientSecret, clientSignature); err != nil {
		return failPair(fmt.Sprintf("Failed to verify client signature: %s", err))
	}

	clientCache.LastPhase = "CLIENTPAIRINGSECRET"
	m.PairingCache.Delete(cacheKey)

	// Pull the fingerprint out of the client certificate and save the AESKey
	// and certificate as a paired client authenticated as the User used
	// to input the PIN on the website.
	//!TODO: Switch to base64 to avoid issues with length limits in some names
	fingerprint := hex.EncodeToString(util.Hash(clientCache.ClientCert.Raw))
	_, err = m.PairingsClient.Apply(context.TODO(), &v1alpha1_apply.PairingApplyConfiguration{
		TypeMetaApplyConfiguration: metav1apply.TypeMetaApplyConfiguration{
			Kind:       ptr.To("Pairing"),
			APIVersion: ptr.To("direwolf.games-on-whales.github.io/v1alpha1"),
		},
		ObjectMetaApplyConfiguration: &metav1apply.ObjectMetaApplyConfiguration{
			Name: ptr.To(fingerprint),
		},
		Spec: &v1alpha1_apply.PairingSpecApplyConfiguration{
			ClientCertPEM: ptr.To(string(pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: clientCache.ClientCert.Raw,
			}))),
			UserReference: &v1alpha1_apply.UserReferenceApplyConfiguration{
				Name: ptr.To(clientCache.Username),
			},
		},
	}, metav1.ApplyOptions{
		FieldManager: "moonlight-proxy",
	})
	if err != nil {
		return failPair("Failed to save pairing: " + err.Error())
	}

	return PairingResponse{Paired: 1, Response: Response{StatusCode: 200}}
}

func aesDecryptECB(ciphertext []byte, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%block.BlockSize() != 0 {
		return nil, errors.New("ciphertext is not a multiple of the block size")
	}

	decrypted := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += block.BlockSize() {
		block.Decrypt(decrypted[i:i+block.BlockSize()], ciphertext[i:i+block.BlockSize()])
	}

	return decrypted, nil
}

func aesEncryptECB(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	if len(plaintext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("plaintext is not a multiple of the block size")
	}

	ciphertext := make([]byte, len(plaintext))
	for i := 0; i < len(plaintext); i += aes.BlockSize {
		block.Encrypt(ciphertext[i:i+aes.BlockSize], plaintext[i:i+aes.BlockSize])
	}

	return ciphertext, nil
}
