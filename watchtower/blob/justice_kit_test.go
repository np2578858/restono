package blob

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func makePubKey() *btcec.PublicKey {
	priv, _ := btcec.NewPrivateKey()
	return priv.PubKey()
}

func makeSig(i int) lnwire.Sig {
	var sigBytes [64]byte
	binary.BigEndian.PutUint64(sigBytes[:8], uint64(i))

	sig, _ := lnwire.NewSigFromWireECDSA(sigBytes[:])
	return sig
}

func makeAddr(size int) []byte {
	addr := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, addr); err != nil {
		panic("unable to create addr")
	}

	return addr
}

type descriptorTest struct {
	name                 string
	encVersion           Type
	decVersion           Type
	sweepAddr            []byte
	revPubKey            *btcec.PublicKey
	delayPubKey          *btcec.PublicKey
	csvDelay             uint32
	commitToLocalSig     lnwire.Sig
	hasCommitToRemote    bool
	commitToRemotePubKey *btcec.PublicKey
	commitToRemoteSig    lnwire.Sig
	encErr               error
	decErr               error
}

var descriptorTests = []descriptorTest{
	{
		name:             "to-local only",
		encVersion:       TypeAltruistCommit,
		decVersion:       TypeAltruistCommit,
		sweepAddr:        makeAddr(22),
		revPubKey:        makePubKey(),
		delayPubKey:      makePubKey(),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:                 "to-local and p2wkh",
		encVersion:           TypeRewardCommit,
		decVersion:           TypeRewardCommit,
		sweepAddr:            makeAddr(22),
		revPubKey:            makePubKey(),
		delayPubKey:          makePubKey(),
		csvDelay:             144,
		commitToLocalSig:     makeSig(1),
		hasCommitToRemote:    true,
		commitToRemotePubKey: makePubKey(),
		commitToRemoteSig:    makeSig(2),
	},
	{
		name:             "unknown encrypt version",
		encVersion:       0,
		decVersion:       TypeAltruistCommit,
		sweepAddr:        makeAddr(34),
		revPubKey:        makePubKey(),
		delayPubKey:      makePubKey(),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
		encErr:           ErrUnknownBlobType,
	},
	{
		name:             "unknown decrypt version",
		encVersion:       TypeAltruistCommit,
		decVersion:       0,
		sweepAddr:        makeAddr(34),
		revPubKey:        makePubKey(),
		delayPubKey:      makePubKey(),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
		decErr:           ErrUnknownBlobType,
	},
	{
		name:             "sweep addr length zero",
		encVersion:       TypeAltruistCommit,
		decVersion:       TypeAltruistCommit,
		sweepAddr:        makeAddr(0),
		revPubKey:        makePubKey(),
		delayPubKey:      makePubKey(),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:             "sweep addr max size",
		encVersion:       TypeAltruistCommit,
		decVersion:       TypeAltruistCommit,
		sweepAddr:        makeAddr(MaxSweepAddrSize),
		revPubKey:        makePubKey(),
		delayPubKey:      makePubKey(),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:             "sweep addr too long",
		encVersion:       TypeAltruistCommit,
		decVersion:       TypeAltruistCommit,
		sweepAddr:        makeAddr(MaxSweepAddrSize + 1),
		revPubKey:        makePubKey(),
		delayPubKey:      makePubKey(),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
		encErr:           ErrSweepAddressToLong,
	},
}

// TestBlobJusticeKitEncryptDecrypt asserts that encrypting and decrypting a
// plaintext blob produces the original. The tests include negative assertions
// when passed invalid combinations, and that all successfully encrypted blobs
// are of constant size.
func TestBlobJusticeKitEncryptDecrypt(t *testing.T) {
	for _, test := range descriptorTests {
		t.Run(test.name, func(t *testing.T) {
			testBlobJusticeKitEncryptDecrypt(t, test)
		})
	}
}

func testBlobJusticeKitEncryptDecrypt(t *testing.T, test descriptorTest) {
	commitmentType, err := test.encVersion.CommitmentType(nil)
	if err != nil {
		require.ErrorIs(t, err, test.encErr)
		return
	}

	breachInfo := &lnwallet.BreachRetribution{
		RemoteDelay: test.csvDelay,
		KeyRing: &lnwallet.CommitmentKeyRing{
			ToLocalKey:    test.delayPubKey,
			ToRemoteKey:   test.commitToRemotePubKey,
			RevocationKey: test.revPubKey,
		},
	}

	kit, err := commitmentType.NewJusticeKit(
		test.sweepAddr, breachInfo, test.hasCommitToRemote,
	)
	if err != nil {
		return
	}
	kit.AddToLocalSig(test.commitToLocalSig)
	kit.AddToRemoteSig(test.commitToRemoteSig)

	// Generate a random encryption key for the blob. The key is
	// sized at 32 byte, as in practice we will be using the remote
	// party's commitment txid as the key.
	var key BreachKey
	_, err = rand.Read(key[:])
	require.NoError(t, err, "unable to generate blob encryption key")

	// Encrypt the blob plaintext using the generated key and
	// target version for this test.
	ctxt, err := Encrypt(kit, key)
	require.ErrorIs(t, err, test.encErr)

	if test.encErr != nil {
		// If the test expected an encryption failure, we can
		// continue to the next test.
		return
	}

	// Ensure that all encrypted blobs are padded out to the same
	// size: 282 bytes for version 0.
	require.Len(t, ctxt, Size(kit))

	// Decrypt the encrypted blob, reconstructing the original
	// blob plaintext from the decrypted contents. We use the target
	// decryption version specified by this test case.
	boj2, err := Decrypt(key, ctxt, test.decVersion)
	require.ErrorIs(t, err, test.decErr)

	if test.decErr != nil {
		// If the test expected an decryption failure, we can
		// continue to the next test.
		return
	}

	// Check that the decrypted blob properly reports whether it has
	// a to-remote output or not.
	if boj2.HasCommitToRemoteOutput() != test.hasCommitToRemote {
		t.Fatalf("expected blob has_to_remote to be %v, got %v",
			test.hasCommitToRemote, boj2.HasCommitToRemoteOutput())
	}

	// Check that the original blob plaintext matches the
	// one reconstructed from the encrypted blob.
	require.Equal(t, kit, boj2)
}

type remoteWitnessTest struct {
	name             string
	blobType         Type
	expWitnessScript func(pk *btcec.PublicKey) []byte
}

// TestJusticeKitRemoteWitnessConstruction tests that a JusticeKit returns the
// proper to-remote witnes script and to-remote witness stack. This should be
// equivalent to p2wkh spend.
func TestJusticeKitRemoteWitnessConstruction(t *testing.T) {
	tests := []remoteWitnessTest{
		{
			name:     "legacy commitment",
			blobType: TypeAltruistCommit,
			expWitnessScript: func(pk *btcec.PublicKey) []byte {
				return pk.SerializeCompressed()
			},
		},
		{
			name:     "anchor commitment",
			blobType: TypeAltruistAnchorCommit,
			expWitnessScript: func(pk *btcec.PublicKey) []byte {
				script, _ := input.CommitScriptToRemoteConfirmed(pk)
				return script
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testJusticeKitRemoteWitnessConstruction(t, test)
		})
	}
}

func testJusticeKitRemoteWitnessConstruction(
	t *testing.T, test remoteWitnessTest) {

	// Generate the to-remote pubkey.
	toRemotePrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	revKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	toLocalKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Sign a message using the to-remote private key. The exact message
	// doesn't matter as we won't be validating the signature's validity.
	digest := bytes.Repeat([]byte("a"), 32)
	rawToRemoteSig := ecdsa.Sign(toRemotePrivKey, digest)

	// Convert the DER-encoded signature into a fixed-size sig.
	commitToRemoteSig, err := lnwire.NewSigFromSignature(rawToRemoteSig)
	require.Nil(t, err)

	commitType, err := test.blobType.CommitmentType(nil)
	require.NoError(t, err)

	breachInfo := &lnwallet.BreachRetribution{
		KeyRing: &lnwallet.CommitmentKeyRing{
			ToRemoteKey:   toRemotePrivKey.PubKey(),
			RevocationKey: revKey.PubKey(),
			ToLocalKey:    toLocalKey.PubKey(),
		},
	}

	justiceKit, err := commitType.NewJusticeKit(nil, breachInfo, true)
	require.NoError(t, err)
	justiceKit.AddToRemoteSig(commitToRemoteSig)

	// Now, compute the to-remote witness script returned by the justice
	// kit.
	_, witness, _, err := justiceKit.ToRemoteOutputSpendInfo()
	require.NoError(t, err)

	// Assert this is exactly the to-remote, compressed pubkey.
	expToRemoteScript := test.expWitnessScript(toRemotePrivKey.PubKey())
	require.Equal(t, expToRemoteScript, witness[1])

	// Compute the expected first element, by appending a sighash all byte
	// to our raw DER-encoded signature.
	rawToRemoteSigWithSigHash := append(
		rawToRemoteSig.Serialize(), byte(txscript.SigHashAll),
	)

	// Assert that the expected witness stack is returned.
	expWitnessStack := [][]byte{
		rawToRemoteSigWithSigHash,
	}
	require.Equal(t, expWitnessStack, witness[:1])
}

// TestJusticeKitToLocalWitnessConstruction tests that a JusticeKit returns the
// proper to-local witness script and to-local witness stack for spending the
// revocation path.
func TestJusticeKitToLocalWitnessConstruction(t *testing.T) {
	csvDelay := uint32(144)

	// Generate the revocation and delay private keys.
	revPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	delayPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Sign a message using the revocation private key. The exact message
	// doesn't matter as we won't be validating the signature's validity.
	digest := bytes.Repeat([]byte("a"), 32)
	rawRevSig := ecdsa.Sign(revPrivKey, digest)

	// Convert the DER-encoded signature into a fixed-size sig.
	commitToLocalSig, err := lnwire.NewSigFromSignature(rawRevSig)
	require.NoError(t, err)

	commitType, err := TypeAltruistCommit.CommitmentType(nil)
	require.NoError(t, err)

	breachInfo := &lnwallet.BreachRetribution{
		RemoteDelay: csvDelay,
		KeyRing: &lnwallet.CommitmentKeyRing{
			RevocationKey: revPrivKey.PubKey(),
			ToLocalKey:    delayPrivKey.PubKey(),
		},
	}

	justiceKit, err := commitType.NewJusticeKit(nil, breachInfo, false)
	require.NoError(t, err)
	justiceKit.AddToLocalSig(commitToLocalSig)

	// Compute the expected to-local script, which is a function of the CSV
	// delay, revocation pubkey and delay pubkey.
	expToLocalScript, err := input.CommitScriptToSelf(
		csvDelay, delayPrivKey.PubKey(), revPrivKey.PubKey(),
	)
	require.NoError(t, err)

	// Compute the to-local script that is returned by the justice kit.
	_, witness, err := justiceKit.ToLocalOutputSpendInfo()
	require.NoError(t, err)

	require.Equal(t, expToLocalScript, witness[2])

	// Compute the expected signature in the bottom element of the stack, by
	// appending a sighash all flag to the raw DER signature.
	rawRevSigWithSigHash := append(
		rawRevSig.Serialize(), byte(txscript.SigHashAll),
	)

	// Finally, validate against our expected witness stack.
	expWitnessStack := [][]byte{
		rawRevSigWithSigHash,
		{1},
	}
	require.Equal(t, expWitnessStack, witness[:2])
}
