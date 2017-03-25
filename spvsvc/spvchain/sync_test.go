package spvchain_test

import (
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/aakselrod/btctestlog"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpctest"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"github.com/btcsuite/btcwallet/spvsvc/spvchain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
)

func TestSetup(t *testing.T) {
	// Create a btcd SimNet node and generate 500 blocks
	h1, err := rpctest.New(&chaincfg.SimNetParams, nil, nil)
	if err != nil {
		t.Fatalf("Couldn't create harness: %v", err)
	}
	defer h1.TearDown()
	err = h1.SetUp(false, 0)
	if err != nil {
		t.Fatalf("Couldn't set up harness: %v", err)
	}
	_, err = h1.Node.Generate(500)
	if err != nil {
		t.Fatalf("Couldn't generate blocks: %v", err)
	}

	// Create a second btcd SimNet node
	h2, err := rpctest.New(&chaincfg.SimNetParams, nil, nil)
	if err != nil {
		t.Fatalf("Couldn't create harness: %v", err)
	}
	defer h2.TearDown()
	err = h2.SetUp(false, 0)
	if err != nil {
		t.Fatalf("Couldn't set up harness: %v", err)
	}

	// Create a third btcd SimNet node and generate 900 blocks
	h3, err := rpctest.New(&chaincfg.SimNetParams, nil, nil)
	if err != nil {
		t.Fatalf("Couldn't create harness: %v", err)
	}
	defer h3.TearDown()
	err = h3.SetUp(false, 0)
	if err != nil {
		t.Fatalf("Couldn't set up harness: %v", err)
	}
	_, err = h3.Node.Generate(900)
	if err != nil {
		t.Fatalf("Couldn't generate blocks: %v", err)
	}

	// Connect, sync, and disconnect h1 and h2
	err = csd([]*rpctest.Harness{h1, h2})
	if err != nil {
		t.Fatalf("Couldn't connect/sync/disconnect h1 and h2: %v", err)
	}

	// Generate 300 blocks on the first node and 350 on the second
	_, err = h1.Node.Generate(300)
	if err != nil {
		t.Fatalf("Couldn't generate blocks: %v", err)
	}
	_, err = h2.Node.Generate(350)
	if err != nil {
		t.Fatalf("Couldn't generate blocks: %v", err)
	}

	// Now we have a node with 800 blocks (h1), 850 blocks (h2), and
	// 900 blocks (h3). The chains of nodes h1 and h2 match up to block
	// 500. By default, a synchronizing wallet connected to all three
	// should synchronize to h3. However, we're going to take checkpoints
	// from h1 at 111, 333, 555, and 777, and add those to the
	// synchronizing wallet's chain parameters so that it should
	// disconnect from h3 at block 111, and from h2 at block 555, and
	// then synchronize to block 800 from h1. Order of connection is
	// unfortunately not guaranteed, so the reorg may not happen with every
	// test.

	// Copy parameters and insert checkpoints
	modParams := chaincfg.SimNetParams
	for _, height := range []int64{111, 333, 555, 777} {
		hash, err := h1.Node.GetBlockHash(height)
		if err != nil {
			t.Fatalf("Couldn't get block hash for height %v: %v",
				height, err)
		}
		modParams.Checkpoints = append(modParams.Checkpoints,
			chaincfg.Checkpoint{
				Hash:   hash,
				Height: int32(height),
			})
	}

	// Create a temporary directory, initialize an empty walletdb with an
	// SPV chain namespace, and create a configuration for the ChainService.
	tempDir, err := ioutil.TempDir("", "spvchain")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)
	db, err := walletdb.Create("bdb", tempDir+"/weks.db")
	defer db.Close()
	if err != nil {
		t.Fatalf("Error opening DB: %v\n", err)
	}
	ns, err := db.Namespace([]byte("weks"))
	if err != nil {
		t.Fatalf("Error geting namespace: %v\n", err)
	}
	config := spvchain.Config{
		DataDir:     tempDir,
		Namespace:   ns,
		ChainParams: modParams,
		AddPeers: []string{
			h3.P2PAddress(),
			h2.P2PAddress(),
			h1.P2PAddress(),
		},
	}

	spvchain.Services = 0
	spvchain.MaxPeers = 3
	spvchain.RequiredServices = wire.SFNodeNetwork
	logger, err := btctestlog.NewTestLogger(t)
	if err != nil {
		t.Fatalf("Could not set up logger: %v", err)
	}
	chainLogger := btclog.NewSubsystemLogger(logger, "CHAIN: ")
	chainLogger.SetLevel(btclog.TraceLvl)
	spvchain.UseLogger(chainLogger) //*/
	svc, err := spvchain.NewChainService(config)
	if err != nil {
		t.Fatalf("Error creating ChainService: %v", err)
	}
	svc.Start()
	defer svc.Stop()

	// Make sure the client synchronizes with the correct node
	err = waitForSync(t, svc, h1, time.Second, 30*time.Second)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %v", err)
	}

	// Generate 150 blocks on h1 to make sure it reorgs the other nodes.
	// Ensure the ChainService instance stays caught up.
	h1.Node.Generate(150)
	err = waitForSync(t, svc, h1, time.Second, 30*time.Second)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %v", err)
	}

	// Connect/sync/disconnect the other nodes to make them reorg to the h1
	// chain.
	/*err = csd([]*rpctest.Harness{h1, h2, h3})
	if err != nil {
		t.Fatalf("Couldn't sync h2 and h3 to h1: %v", err)
	}*/
}

// csd does a connect-sync-disconnect between nodes in order to support
// reorg testing. It brings up and tears down a temporary node, otherwise the
// nodes try to reconnect to each other which results in unintended reorgs.
func csd(harnesses []*rpctest.Harness) error {
	hTemp, err := rpctest.New(&chaincfg.SimNetParams, nil, nil)
	if err != nil {
		return err
	}
	// Tear down node at the end of the function.
	defer hTemp.TearDown()
	err = hTemp.SetUp(false, 0)
	if err != nil {
		return err
	}
	for _, harness := range harnesses {
		err = rpctest.ConnectNode(hTemp, harness)
		if err != nil {
			return err
		}
	}
	return rpctest.JoinNodes(harnesses, rpctest.Blocks)
}

// waitForSync waits for the ChainService to sync to the current chain state.
func waitForSync(t *testing.T, svc *spvchain.ChainService,
	correctSyncNode *rpctest.Harness, checkInterval,
	timeout time.Duration) error {
	knownBestHash, knownBestHeight, err :=
		correctSyncNode.Node.GetBestBlock()
	if err != nil {
		return err
	}
	t.Logf("Syncing to %v (%v)", knownBestHeight, knownBestHash)
	var haveBest *waddrmgr.BlockStamp
	haveBest, err = svc.BestSnapshot()
	if err != nil {
		return err
	}
	var total time.Duration
	for haveBest.Hash != *knownBestHash {
		if total > timeout {
			return fmt.Errorf("Timed out after %v.", timeout)
		}
		if haveBest.Height > knownBestHeight {
			return fmt.Errorf("Synchronized to the wrong chain.")
		}
		time.Sleep(checkInterval)
		total += checkInterval
		haveBest, err = svc.BestSnapshot()
		if err != nil {
			return fmt.Errorf("Couldn't get best snapshot from "+
				"ChainService: %v", err)
		}
		t.Logf("Synced to %v (%v)", haveBest.Height, haveBest.Hash)
	}
	// Check if we're current
	if !svc.IsCurrent() {
		return fmt.Errorf("ChainService doesn't see itself as current!")
	}
	return nil
}