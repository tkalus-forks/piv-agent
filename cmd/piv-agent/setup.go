package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/go-piv/piv-go/piv"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
)

// SetupCmd represents the setup command.
type SetupCmd struct {
	Card             string `kong:"help='Specify a smart card device'"`
	ResetSecurityKey bool   `kong:"help='Overwrite any existing keys'"`
	PIN              uint64 `kong:"help='Set the PIN/PUK of the device (6-8 digits). Will be prompted interactively if not provided.'"`
	AllTouchPolicies bool   `kong:"default='true',help='Create two additional keys with touch policies always and never (default true)'"`
}

type slotSpec struct {
	slot        piv.Slot
	touchPolicy piv.TouchPolicy
}

var allKeySpec = []slotSpec{
	{piv.SlotAuthentication, piv.TouchPolicyCached},
	{piv.SlotSignature, piv.TouchPolicyAlways},
	{piv.SlotCardAuthentication, piv.TouchPolicyNever},
}

var touchStringMap = map[piv.TouchPolicy]string{
	piv.TouchPolicyNever:  "never",
	piv.TouchPolicyAlways: "always",
	piv.TouchPolicyCached: "cached",
}

type sshPubKeySpec struct {
	pubKey      ssh.PublicKey
	slot        piv.Slot
	touchPolicy piv.TouchPolicy
	card        string
	serial      uint32
}

func interactivePIN() (uint64, error) {
	fmt.Print("Enter a new PIN/PUK (6-8 digits): ")
	rawPIN, err := terminal.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return 0, fmt.Errorf("couldn't read PIN/PUK: %w", err)
	}
	pin, err := strconv.ParseUint(string(rawPIN), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid characters: %w", err)
	}
	fmt.Print("Repeat PIN/PUK: ")
	repeat, err := terminal.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return 0, fmt.Errorf("couldn't read PIN/PUK: %w", err)
	}
	if !bytes.Equal(repeat, rawPIN) {
		return 0, fmt.Errorf("PIN/PUK entries not equal")
	}
	return pin, nil
}

// Run the setup command to configure a security key.
func (cmd *SetupCmd) Run() error {
	log, err := zap.NewDevelopment()
	if err != nil {
		return fmt.Errorf("couldn't init logger: %w", err)
	}
	defer log.Sync()
	// if PIN has not been specified, ask interactively
	if cmd.PIN == 0 {
		cmd.PIN, err = interactivePIN()
		if err != nil {
			return err
		}
	}
	if cmd.PIN < 100000 || cmd.PIN > 99999999 {
		return fmt.Errorf("invalid PIN, must be 6-8 digits")
	}
	k, err := getSecurityKey(cmd.Card)
	if err != nil {
		return fmt.Errorf("couldn't get security key: %w", err)
	}
	return cmd.setup(k)
}

func (cmd *SetupCmd) setup(k *piv.YubiKey) error {
	_, err := k.Certificate(piv.SlotAuthentication)
	if err == nil {
		if !cmd.ResetSecurityKey {
			return fmt.Errorf("security key already set up and --reset-security-key not specified")
		}
		if err = k.Reset(); err != nil {
			return fmt.Errorf("couldn't reset security key: %w", err)
		}
	} else if !errors.Is(err, piv.ErrNotFound) {
		return fmt.Errorf("couldn't get certificate: %w", err)
	}
	var mk [24]byte
	if _, err := rand.Read(mk[:]); err != nil {
		return fmt.Errorf("couldn't get random bytes: %w", err)
	}
	if err = k.SetManagementKey(piv.DefaultManagementKey, mk); err != nil {
		return fmt.Errorf("couldn't set management key: %w", err)
	}
	if err = k.SetMetadata(mk, &piv.Metadata{ManagementKey: &mk}); err != nil {
		return fmt.Errorf("couldn't store management key: %w", err)
	}
	pin := strconv.FormatUint(cmd.PIN, 10)
	if err = k.SetPIN(piv.DefaultPIN, pin); err != nil {
		return fmt.Errorf("couldn't set PIN: %w", err)
	}
	if err = k.SetPUK(piv.DefaultPUK, pin); err != nil {
		return fmt.Errorf("couldn't set PUK: %w", err)
	}
	keySpec := []slotSpec{
		{piv.SlotAuthentication, piv.TouchPolicyCached},
	}
	if cmd.AllTouchPolicies {
		keySpec = allKeySpec
	}
	for _, ss := range keySpec {
		if err = cmd.configureSlot(k, mk, ss.slot, ss.touchPolicy); err != nil {
			return fmt.Errorf("couldn't configure slot %v: %w", ss.slot, err)
		}
	}
	return nil
}

func (cmd *SetupCmd) configureSlot(k *piv.YubiKey, mk [24]byte,
	slot piv.Slot, touchPolicy piv.TouchPolicy) error {
	pub, err := k.GenerateKey(mk, slot, piv.Key{
		Algorithm:   piv.AlgorithmEC256,
		PINPolicy:   piv.PINPolicyOnce,
		TouchPolicy: touchPolicy,
	})
	if err != nil {
		return fmt.Errorf("couldn't generate key: %w", err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("couldn't generate parent key: %w", err)
	}
	parent := &x509.Certificate{
		Subject: pkix.Name{
			Organization:       []string{"piv-agent"},
			OrganizationalUnit: []string{version},
		},
		PublicKey: priv.Public(),
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("couldn't generate serial: %w", err)
	}
	template := &x509.Certificate{
		Subject: pkix.Name{
			CommonName: "SSH key",
		},
		NotAfter:     time.Now().AddDate(64, 0, 0),
		NotBefore:    time.Now(),
		SerialNumber: serial,
		KeyUsage:     x509.KeyUsageKeyAgreement | x509.KeyUsageDigitalSignature,
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, template, parent, pub, priv)
	if err != nil {
		return fmt.Errorf("couldn't create certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(certBytes)
	if err != nil {
		return fmt.Errorf("couldn't parse certificate: %w", err)
	}
	if err = k.SetCertificate(mk, slot, cert); err != nil {
		return fmt.Errorf("couldn't set certificate: %w", err)
	}
	sshKey, err := ssh.NewPublicKey(pub)
	if err != nil {
		return fmt.Errorf("couldn't get SSH public key: %w", err)
	}
	fmt.Printf("🔑 Generated SSH key, touch policy: %s\n",
		touchStringMap[touchPolicy])
	fmt.Printf(string(ssh.MarshalAuthorizedKey(sshKey)))
	return nil
}
