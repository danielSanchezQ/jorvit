//$(which go) run $0 $@; exit $?

package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gocarina/gocsv"
	"github.com/input-output-hk/jorvit/internal/datastore"
	"github.com/input-output-hk/jorvit/internal/kit"
	"github.com/input-output-hk/jorvit/internal/loader"
	"github.com/input-output-hk/jorvit/internal/wallet"
	"github.com/input-output-hk/jorvit/internal/webproxy"
	"github.com/rinor/jorcli/jcli"
	"github.com/rinor/jorcli/jnode"
	"github.com/skip2/go-qrcode"
	"golang.org/x/crypto/blake2b"
)

// Version and build info
var (
	Version    = "dev"
	CommitHash = "none"
	BuildDate  = "unknown"
)

type bftLeader struct {
	sk      string
	pk      string
	acc     string
	skFile  string
	cfgFile string
}

type jcliProposal struct {
	ExternalID string `json:"external_id"`
	Options    uint8  `json:"options"`
	Action     string `json:"action"`
}

type jcliVotePlan struct {
	Payload      string         `json:"payload_type"`
	VoteStart    ChainTime      `json:"vote_start"`
	VoteEnd      ChainTime      `json:"vote_end"`
	CommitteeEnd ChainTime      `json:"committee_end"`
	Proposals    []jcliProposal `json:"proposals"`
	VotePlanID   string         `json:"-"`
	Certificate  string         `json:"-"`
}

type ChainTime struct {
	Epoch  int64 `json:"epoch"`
	SlotID int64 `json:"slot_id"`
}

func (ct ChainTime) String() string {
	return strconv.FormatInt(ct.Epoch, 10) + "." + strconv.FormatInt(ct.SlotID, 10)
}

func ToChainTime(block0Time int64, SlotDuration uint8, SlotsPerEpoch uint32, dataTime int64) ChainTime {
	slotsTotal := (dataTime - block0Time) / int64(SlotDuration)
	epoch := slotsTotal / int64(SlotsPerEpoch)
	slot := slotsTotal % int64(SlotsPerEpoch)

	return ChainTime{
		Epoch:  epoch,
		SlotID: slot,
	}
}

var (
	votePlanProposalsMax = 255
	wallets              []wallet.Wallet // = wallet.SampleWallets()
)

var (
	proposals datastore.ProposalsStore
	funds     datastore.FundsStore
)

func timeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	log.Printf("%s took %s", name, elapsed)
}

func loadProposals(file string) error {
	defer timeTrack(time.Now(), "Proposals File load")
	proposals = &datastore.Proposals{}
	return proposals.Initialize(file)
}

func loadFundInfo(file string) error {
	defer timeTrack(time.Now(), "Fund File load")
	funds = &datastore.Funds{}
	return funds.Initialize(file)
}

type sliceFlag []string

func (sf *sliceFlag) String() string {
	return strings.Join(*sf, ",")
}

func (sf *sliceFlag) Set(val string) error {
	*sf = append(*sf, val)
	return nil
}

func main() {
	var (
		err             error
		bftLeaderKey    sliceFlag
		globalCommittee sliceFlag
	)

	// node settings
	proxyAddrPort := flag.String("proxy", "0.0.0.0:8000", "Address where REST api PROXY should listen in IP:PORT format")
	restAddrPort := flag.String("rest", "0.0.0.0:8001", "Address where Jörmungandr REST api should listen in IP:PORT format")
	nodeAddrPort := flag.String("node", "127.0.0.1:9001", "Address where Jörmungandr node should listen in IP:PORT format")
	explorerEnabled := flag.Bool("explorer", false, "Enable/Disable explorer")
	restCorsAllowed := flag.String("cors", "https://api.vit.iohk.io,https://127.0.0.1,http://127.0.0.1,http://127.0.0.1:8000,http://127.0.0.1:8001,https://localhost,http://localhost,http://localhost:8000,http://localhost:8001,http://0.0.0.0:8000,http://0.0.0.0:8001", "Comma separated list of CORS allowed origins")
	skipBootstrap := flag.Bool("skip-bootstrap", true, "Skip node bootstrap, in case of first/single genesis leader (default true)")

	// external proposal data
	proposalsPath := flag.String("proposals", "."+string(os.PathSeparator)+"assets"+string(os.PathSeparator)+"proposals.csv", "CSV full path (filename) to load PROPOSALS from")
	fundsPath := flag.String("fund", "."+string(os.PathSeparator)+"assets"+string(os.PathSeparator)+"fund.csv", "CSV full path (filename) to load FUND info from")
	dumbGenesisDataPath := flag.String("dumbdata", "."+string(os.PathSeparator)+"assets"+string(os.PathSeparator)+"dumb_genesis_data.yaml", "YAML full path (filename) to load dumb genesis funds from")

	// vote and committee related timing
	voteStartFlag := flag.String("vote-start", "", "Vote start time in '2006-01-02T15:04:05Z07:00' RFC3339 format. If not set 'genesis-time' will be used")
	voteEndFlag := flag.String("vote-end", "", "Vote end time in '2006-01-02T15:04:05Z07:00' RFC3339 format. If not set 'vote-duration' will be used")
	committeeEndFlag := flag.String("committee-end", "", "Committee end time in '2006-01-02T15:04:05Z07:00' RFC3339 format. If not set 'committee-duration' will be used")

	voteDurationFlag := flag.String("vote-duration", "144h", "Voting period duration. Ignored if 'vote-end' is set")
	committeeDurationFlag := flag.String("committee-duration", "24h", "Committee period duration. Ignored if 'committee-end' is set")

	block0Voteplans := flag.Bool("block0-voteplan", false, "Enable/Disable inclusion of proposals/voteplans signed certificate on block0")

	// genesis (block0) settings
	genesisTimeFlag := flag.String("genesis-time", "", "Genesis time in '2006-01-02T15:04:05Z07:00' RFC3339 format (default \"Now()\")")
	slotDurFlag := flag.String("slot-duration", "20s", "Slot period duration. 1s-255s")
	epochDurFlag := flag.String("epoch-duration", "24h", "Epoch period duration")

	// BFT Leaders - also promoted to Global Committee members
	bftLeaderTot := flag.Uint("bft-leader-tot", 1, "Number of BFT Leaders. NEW SK/PK key pair(s) will be autogenerated. min: 1 if \"bft-leader-key\" not set, otherwise this will be ignored")
	flag.Var(&bftLeaderKey, "bft-leader-key", "File containing SK to be used as BFT leader. If not set, \"bft-leader-tot\" will be used to generate keys")

	// Global Committee members
	flag.Var(&globalCommittee, "global-committee", "Global committee member public key. ex: ed25519_pk15f7p4nzektlrj6muvvmn0hatzekg7yf0qjx54pg72qq2zgjjzdzqwhm8rz")

	// (bug) - 0 fees is ignored from the jorcli lib (needs fixing)
	// fees
	feesCertificate := flag.Uint64("fees-certificate", 0, "Default certificate fee (lovelace)")
	feesCoefficient := flag.Uint64("fees-coefficient", 0, "Coefficient fee")
	feesConstant := flag.Uint64("fees-constant", 0, "Constant fee (lovelace)")
	feesCertificatePoolRegistration := flag.Uint64("fees-certificate-pool-registration", 0, "Pool registration certificate fee (lovelace)")
	feesCertificateStakeDelegation := flag.Uint64("fees-certificate-stake-delegation", 0, "Stake delegation certificate fee (lovelace)")
	feesCertificateVotePlan := flag.Uint64("fees-certificate-vote-plan", 0, "VotePlan certificate fee (lovelace)")
	feesCertificateVoteCast := flag.Uint64("fees-certificate-vote-cast", 0, "VoteCast certificate fee (lovelace)")
	feesGoTo := flag.String("fees-go-to", "rewards", "Where to send the collected fees, rewards or treasury")

	// extra
	allowNodeRestart := flag.Bool("allow-node-restart", true, "Allows to stop the node started from the service and restart it manually")
	shutdownNode := flag.Bool("shutdown-node", true, "When exiting try node shutdown in case the node was restarted manually")
	startNode := flag.Bool("start-node", true, "Start jörmungandr node. When false only config will be generated")
	dateTimeFormat := flag.String("time-format", time.RFC3339, "Date/Time format that will be used for display (go lang format), ex: \"2006-01-02 15:04:05 -0700 MST\"")

	// Dump raw data
	dumpRaw := flag.String("dump-raw", "", "Dump raw data like voteplan.json, voteplan.cert, funds.csv, voteplans.csv, proposals.csv")

	// version info
	version := flag.Bool("version", false, "Prints current app version and build info")

	// fund each btf leader account address with this value
	leaderFund := uint64(10_000_000_001)

	flag.Parse()

	if *version {
		fmt.Printf("Version - %s\n", Version)
		fmt.Printf("Commit  - %s\n", CommitHash)
		fmt.Printf("Date    - %s\n", BuildDate)
		os.Exit(0)
	}

	// check if file exist - duplicate check is performed later on
	for i := range bftLeaderKey {
		_, err = os.Stat(bftLeaderKey[i])
		kit.FatalOn(err)
	}

	// Set bftLeaderTot
	if len(bftLeaderKey) > 0 {
		*bftLeaderTot = uint(len(bftLeaderKey))
	}

	if *dumpRaw != "" {
		*dumpRaw, err = filepath.Abs(*dumpRaw)
		kit.FatalOn(err)
	}

	if *dateTimeFormat == "" {
		*dateTimeFormat = time.RFC3339
	}

	if *genesisTimeFlag == "" {
		*genesisTimeFlag = time.Now().UTC().Format(time.RFC3339)
	}
	genesisTime, err := time.Parse(time.RFC3339, *genesisTimeFlag)
	kit.FatalOn(err, "genesisTime")

	slotDur, err := time.ParseDuration(*slotDurFlag)
	kit.FatalOn(err, "slotDuration")
	switch {
	case slotDur == 0:
		log.Fatalf("[%s] - cannot be 0", "slotDuration")
	case slotDur%time.Second > 0:
		log.Fatalf("[%s] - smallest unit is [1s]", "slotDuration")
	case slotDur > 255*time.Second:
		log.Fatalf("[%s] - max allowed value is [255s]", "slotDuration")
	}

	epochDur, err := time.ParseDuration(*epochDurFlag)
	kit.FatalOn(err, "epochDuration")
	switch {
	case epochDur == 0:
		log.Fatalf("[%s] - cannot be 0", "epochDuration")
	case epochDur%time.Second > 0:
		log.Fatalf("[%s] - smallest unit is [1s]", "epochDuration")
	case epochDur%slotDur > 0:
		log.Fatalf("[%s: %s] - should be multiple of [%s: %s].", "epochDuration", epochDur.String(), "SlotDuration", slotDur.String())
	}

	voteDur, err := time.ParseDuration(*voteDurationFlag)
	kit.FatalOn(err, "voteDuration")
	switch {
	case voteDur == 0:
		log.Fatalf("[%s] - cannot be 0", "voteDuration")
	case voteDur%time.Second > 0:
		log.Fatalf("[%s] - smallest unit is [1s]", "voteDuration")
	case voteDur%slotDur > 0:
		log.Fatalf("[%s: %s] - should be multiple of [%s: %s].", "voteDuration", voteDur.String(), "SlotDuration", slotDur.String())
	}

	committeeDur, err := time.ParseDuration(*committeeDurationFlag)
	kit.FatalOn(err, "committeeDuration")
	switch {
	case committeeDur == 0:
		log.Fatalf("[%s] - cannot be 0", "committeeDuration")
	case committeeDur%time.Second > 0:
		log.Fatalf("[%s] - smallest unit is [1s]", "committeeDuration")
	case committeeDur%slotDur > 0:
		log.Fatalf("[%s: %s] - should be multiple of [%s: %s].", "committeeDuration", committeeDur.String(), "SlotDuration", slotDur.String())
	}

	if *voteStartFlag == "" {
		*voteStartFlag = *genesisTimeFlag
	}
	voteStartTime, err := time.Parse(time.RFC3339, *voteStartFlag)
	kit.FatalOn(err, "voteStartTime")
	switch {
	case voteStartTime.Sub(genesisTime) < 0:
		log.Fatalf("%s: [%s] can't be smaller than %s: [%s]", "voteStart", *voteStartFlag, "genesisTime", *genesisTimeFlag)
	case voteStartTime.Sub(genesisTime)%slotDur != 0:
		log.Fatalf("%s: [%s] needs to have %s: [%s] steps from %s: [%s]", "voteStart", *voteStartFlag, "SlotDuration", slotDur.String(), "genesisTime", *genesisTimeFlag)
	}

	if *voteEndFlag == "" {
		*voteEndFlag = voteStartTime.Add(voteDur).Format(time.RFC3339)
	}
	voteEndTime, err := time.Parse(time.RFC3339, *voteEndFlag)
	kit.FatalOn(err, "voteEndTime")
	switch {
	case voteEndTime.Sub(voteStartTime) < 0:
		log.Fatalf("%s: [%s] can't be smaller than %s: [%s]", "voteEnd", *voteEndFlag, "voteStart", *voteStartFlag)
	case voteEndTime.Sub(genesisTime)%slotDur != 0:
		log.Fatalf("%s: [%s] needs to have %s: [%s] steps from %s: [%s]", "voteEnd", *voteEndFlag, "SlotDuration", slotDur.String(), "genesisTime", *genesisTimeFlag)
	}

	if *committeeEndFlag == "" {
		*committeeEndFlag = voteEndTime.Add(committeeDur).Format(time.RFC3339)
	}
	committeeEndTime, err := time.Parse(time.RFC3339, *committeeEndFlag)
	kit.FatalOn(err, "committeeEndTime")
	switch {
	case committeeEndTime.Sub(voteEndTime) < 0:
		log.Fatalf("%s: [%s] can't be smaller than %s: [%s]", "committeeEnd", *committeeEndFlag, "voteEnd", *voteEndFlag)
	case committeeEndTime.Sub(genesisTime)%slotDur != 0:
		log.Fatalf("%s: [%s] needs to have %s: [%s] steps from %s: [%s]", "committeeEnd", *committeeEndFlag, "SlotDuration", slotDur.String(), "genesisTime", *genesisTimeFlag)
	}

	voteStart := ToChainTime(
		genesisTime.Unix(),
		uint8(slotDur.Seconds()),
		uint32(epochDur/slotDur),
		voteStartTime.Unix(),
	)

	voteEnd := ToChainTime(
		genesisTime.Unix(),
		uint8(slotDur.Seconds()),
		uint32(epochDur/slotDur),
		voteEndTime.Unix(),
	)

	committeeEnd := ToChainTime(
		genesisTime.Unix(),
		uint8(slotDur.Seconds()),
		uint32(epochDur/slotDur),
		committeeEndTime.Unix(),
	)

	switch {
	case *proposalsPath == "":
		log.Fatalf("[%s] - not provided", "proposals file")
	case *fundsPath == "":
		log.Fatalf("[%s] - not provided", "fund file")
		//
	case *bftLeaderTot == 0:
		log.Fatalf("[%s: %d] - wrong value", "bftLeaderTot", *bftLeaderTot)
		//
	case *proxyAddrPort == "":
		log.Fatalf("[%s] - not set", "proxy")
	case *restAddrPort == "":
		log.Fatalf("[%s] - not set", "rest")
	case *nodeAddrPort == "":
		log.Fatalf("[%s] - not set", "node")
	}

	nodeListen := strings.Split(*nodeAddrPort, ":")
	nodeAddr := nodeListen[0]
	nodePort, err := strconv.Atoi(nodeListen[1])
	kit.FatalOn(err, "nodePort")

	err = loadProposals(*proposalsPath)
	kit.FatalOn(err, "loadProposals")

	err = loadFundInfo(*fundsPath)
	kit.FatalOn(err, "loadFundInfo")

	var (
		// Proxy
		proxyAddress = *proxyAddrPort

		// Rest
		restAddress = *restAddrPort

		// P2P
		p2pIPver, p2pProto           = "ip4", "tcp"
		p2pListenAddr, p2pListenPort = nodeAddr, nodePort
		p2pListenAddress             = "/" + p2pIPver + "/" + p2pListenAddr + "/" + p2pProto + "/" + strconv.Itoa(p2pListenPort)

		// General
		consensus      = "bft" // bft or genesis_praos
		discrimination = ""    // "" (empty defaults to "production")

		// Node config log
		nodeCfgLogLevel = "info"
	)

	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	kit.FatalOn(err)

	// Check for jcli binary. Local folder first (jor_bins), then PATH
	jcliBin, err := kit.FindExecutable("jcli", "jor_bins")
	kit.FatalOn(err, jcliBin)
	jcli.BinName(jcliBin)

	// get jcli version
	jcliVersion, err := jcli.VersionFull()
	kit.FatalOn(err, kit.B2S(jcliVersion))

	// create a new temporary directory inside your systems temp dir
	workingDir, err := ioutil.TempDir(dir, "jnode_VIT_")
	kit.FatalOn(err, "workingDir")
	log.Printf("Working Directory: %s", workingDir)

	/* BFT LEADER(s) */

	leaders := make([]bftLeader, 0, *bftLeaderTot)
	leadersPubKey := make(map[string]bool, *bftLeaderTot)

	for i := 0; uint(i) < *bftLeaderTot; i++ {
		var leaderSK []byte

		if len(bftLeaderKey) > 0 {
			leaderSK, err = ioutil.ReadFile(bftLeaderKey[i])
			kit.FatalOn(err, kit.B2S(leaderSK))
		} else {
			leaderSK, err = jcli.KeyGenerate("", "Ed25519", "")
			kit.FatalOn(err, kit.B2S(leaderSK))
		}

		leaderPK, err := jcli.KeyToPublic(leaderSK, "", "")
		kit.FatalOn(err, kit.B2S(leaderPK))

		if leadersPubKey[kit.B2S(leaderPK)] {
			log.Printf("Duplicate BFT Leader skip: %s", kit.B2S(leaderPK))
			continue
		}
		leadersPubKey[kit.B2S(leaderPK)] = true

		leaderACC, err := jcli.AddressAccount(kit.B2S(leaderPK), "", "")
		kit.FatalOn(err, kit.B2S(leaderACC))

		// Needed later on to sign
		bftSecretFile := filepath.Join(workingDir, strconv.Itoa(i)+"_bft_secret.key")
		err = ioutil.WriteFile(bftSecretFile, leaderSK, 0744)
		kit.FatalOn(err)

		leaders = append(leaders, bftLeader{
			sk:     kit.B2S(leaderSK),
			pk:     kit.B2S(leaderPK),
			acc:    kit.B2S(leaderACC),
			skFile: bftSecretFile,
		})
	}

	/////////////////////
	//  block0 config  //
	/////////////////////

	block0cfg := jnode.NewBlock0Config()

	block0Discrimination := "production"
	if discrimination == "testing" {
		block0Discrimination = "test"
	}

	// set/change config params
	block0cfg.BlockchainConfiguration.Block0Date = genesisTime.Unix()
	block0cfg.BlockchainConfiguration.Block0Consensus = consensus
	block0cfg.BlockchainConfiguration.Discrimination = block0Discrimination

	block0cfg.BlockchainConfiguration.SlotDuration = uint8(slotDur.Seconds())
	block0cfg.BlockchainConfiguration.SlotsPerEpoch = uint32(epochDur / slotDur)

	block0cfg.BlockchainConfiguration.LinearFees.Certificate = *feesCertificate
	block0cfg.BlockchainConfiguration.LinearFees.Coefficient = *feesCoefficient
	block0cfg.BlockchainConfiguration.LinearFees.Constant = *feesConstant

	block0cfg.BlockchainConfiguration.LinearFees.PerCertificateFees.CertificatePoolRegistration = *feesCertificatePoolRegistration
	block0cfg.BlockchainConfiguration.LinearFees.PerCertificateFees.CertificateStakeDelegation = *feesCertificateStakeDelegation

	block0cfg.BlockchainConfiguration.LinearFees.PerVoteCertificateFees.CertificateVoteCast = *feesCertificateVoteCast
	block0cfg.BlockchainConfiguration.LinearFees.PerVoteCertificateFees.CertificateVotePlan = *feesCertificateVotePlan

	block0cfg.BlockchainConfiguration.FeesGoTo = *feesGoTo

	// Bft Leader
	for i := range leaders {
		err = block0cfg.AddConsensusLeader(leaders[i].pk)
		kit.FatalOn(err)
	}

	// Global Committee Members list
	committeePubKey := make(map[string]bool, len(globalCommittee))
	for i := range globalCommittee {
		// Check if committee pk is on bft leaders
		if leadersPubKey[globalCommittee[i]] {
			log.Printf("Duplicate Committee member on BFT Leader skip: %s", globalCommittee[i])
			continue
		}

		if committeePubKey[globalCommittee[i]] {
			log.Printf("Duplicate Committee member skip: %s", globalCommittee[i])
			continue
		}
		committeePubKey[globalCommittee[i]] = true

		pk, err := jcli.KeyToBytes([]byte(globalCommittee[i]), "", "")
		kit.FatalOn(err, kit.B2S(pk))

		block0cfg.AddCommittee(kit.B2S(pk))
	}

	// add legacy funds
	for i := range wallets {
		wallets[i].Totals = 0
		for _, lf := range wallets[i].Funds {
			err = block0cfg.AddInitialLegacyFund(lf.Address, lf.Value)
			kit.FatalOn(err)
			wallets[i].Totals += lf.Value
		}
	}

	// fund bft leader(s) account so at least we have some funds (10K ADA)
	for i := range leaders {
		err = block0cfg.AddInitialFund(leaders[i].acc, leaderFund)
		kit.FatalOn(err)
	}

	// Proposals list per payload type
	payloadProposals := make(map[string][]*loader.ProposalData)
	for _, p := range *proposals.All() {
		payloadProposals[p.VoteType] = append(payloadProposals[p.VoteType], p)
	}

	// Calculate nr of needed voteplans since there is a limit of proposals a plan can have (256)
	// Taking in consideration also payload (although we have only public for now)
	vpNeeded := 0 //votePlansNeeded(proposalsTot, votePlanProposalsMax)
	for _, vpp := range payloadProposals {
		vpNeeded += votePlansNeeded(len(vpp), votePlanProposalsMax)
	}

	jcliVotePlans := make([]jcliVotePlan, vpNeeded)
	funds.First().VotePlans = make([]loader.ChainVotePlan, vpNeeded)

	for pt := range payloadProposals {

		// Generate proposals hash and associate it to a voteplan
		for i, proposal := range payloadProposals[pt] {

			// retrieve the voteplan intenal index based on the proposal index we are at
			vpi := i / votePlanProposalsMax

			// tmp - hash the proposal (TODO: decide what to hash in production, file bytes ???)
			externalID := blake2b.Sum256([]byte(proposal.Proposal.ID + proposal.InternalID))
			proposal.ChainProposal.ExternalID = hex.EncodeToString(externalID[:])

			// add proposal hash to the respective voteplan internal container
			jcliVotePlans[vpi].Proposals = append(
				jcliVotePlans[vpi].Proposals,
				jcliProposal{
					ExternalID: proposal.ChainProposal.ExternalID,
					Options:    uint8(len(proposal.ChainProposal.VoteOptions)),
					Action:     proposal.VoteAction,
				},
			)
			// Set payload once
			if jcliVotePlans[vpi].Payload == "" {
				jcliVotePlans[vpi].Payload = pt
			}

		}
	}

	signersFiles := make([]string, 0, len(leaders))
	signersFiles = append(signersFiles, leaders[0].skFile) // cert accepts only 1 signer for now....
	/*
		for i := range leaders {
			signersFiles = append(signersFiles, leaders[i].skFile)
		}
	*/

	// Generate voteplan certificates and id
	for i := range jcliVotePlans {

		jcliVotePlans[i].VoteStart = voteStart
		jcliVotePlans[i].VoteEnd = voteEnd
		jcliVotePlans[i].CommitteeEnd = committeeEnd

		stdinConfig, err := json.MarshalIndent(jcliVotePlans[i], "", " ")
		kit.FatalOn(err, "json.Marshal VotePlan Config")

		cert, err := jcli.CertificateNewVotePlan(stdinConfig, "", "")
		kit.FatalOn(err, "CertificateNewVotePlan", kit.B2S(cert))

		id, err := jcli.CertificateGetVotePlanID(cert, "", "")
		kit.FatalOn(err, "CertificateGetVotePlanID:", kit.B2S(id))

		if *dumpRaw != "" {
			vpj, err := os.Create(filepath.Join(*dumpRaw, "voteplan_"+kit.B2S(id)+".json"))
			kit.FatalOn(err, "VotePlan json CREATE", kit.B2S(id))
			_, err = vpj.Write(stdinConfig)
			kit.FatalOn(err, "VotePlan json WRITE", kit.B2S(id))
			err = vpj.Close()
			kit.FatalOn(err, "VotePlan json CLOSE", kit.B2S(id))

			vpc, err := os.Create(filepath.Join(*dumpRaw, "voteplan_"+kit.B2S(id)+".cert"))
			kit.FatalOn(err, "VotePlan cert CREATE", kit.B2S(id))
			_, err = vpc.Write(cert)
			kit.FatalOn(err, "VotePlan cert WRITE", kit.B2S(id))
			err = vpc.Close()
			kit.FatalOn(err, "VotePlan cert CLOSE", kit.B2S(id))
		}

		cert, err = jcli.CertificateSign(cert, signersFiles, "", "")
		kit.FatalOn(err, "CertificateSign:", kit.B2S(cert))

		jcliVotePlans[i].Certificate = kit.B2S(cert)
		jcliVotePlans[i].VotePlanID = kit.B2S(id)

		// Update Fund info with VotePlans Data
		funds.First().VotePlans[i].VotePlanID = jcliVotePlans[i].VotePlanID
		funds.First().VotePlans[i].VoteStart = voteStartTime.Format(*dateTimeFormat)
		funds.First().VotePlans[i].VoteEnd = voteEndTime.Format(*dateTimeFormat)
		funds.First().VotePlans[i].CommitteeEnd = committeeEndTime.Format(*dateTimeFormat)
		funds.First().VotePlans[i].Payload = jcliVotePlans[i].Payload

		funds.First().VotePlans[i].FundID = funds.First().FundID
		funds.First().VotePlans[i].VpInternalID = strconv.Itoa(i + 1)

		// Update proposals index and voteplan
		for pi, prop := range jcliVotePlans[i].Proposals {
			// TODO: fix this search
			proposal := datastore.FilterSingle(
				proposals.All(),
				func(v *loader.ProposalData) bool {
					return v.ChainProposal.ExternalID == prop.ExternalID
				},
			)

			proposal.ChainProposal.Index = uint8(pi)
			proposal.ChainVotePlan = &(funds.First().VotePlans[i])
		}

		if *block0Voteplans {
			// Vote Plans add certificate to block0
			err = block0cfg.AddInitialCertificate(jcliVotePlans[i].Certificate)
			kit.FatalOn(err, "AddInitialCertificate")
		}
	}
	//////////////////////////////////////////////
	/* TODO: TMP - remove once properly defined */
	if funds.First().StartTime == "" {
		funds.First().StartTime = voteStartTime.Format(*dateTimeFormat)
	}
	if funds.First().EndTime == "" {
		funds.First().EndTime = voteEndTime.Format(*dateTimeFormat)
	}
	if funds.First().VotingPowerInfo == "" {
		funds.First().VotingPowerInfo = funds.First().StartTime
	}
	if funds.First().RewardsInfo == "" {
		funds.First().RewardsInfo = committeeEndTime.Add(epochDur).Format(*dateTimeFormat)
	}
	if funds.First().NextStartTime == "" {
		funds.First().NextStartTime = committeeEndTime.Add(30 * epochDur).Format(*dateTimeFormat)
	}
	/* TODO: TMP - remove once properly defined */
	//////////////////////////////////////////////

	if *dumpRaw != "" {
		// FUNDS
		fundsFile, err := os.Create(filepath.Join(*dumpRaw, "sql_funds.csv"))
		kit.FatalOn(err, "Funds csv CREATE")
		f := []*loader.FundData{funds.First()}
		err = gocsv.MarshalFile(&f, fundsFile) // Use this to save the CSV back to the file
		kit.FatalOn(err, "Funds csv WRITE")
		err = fundsFile.Close()
		kit.FatalOn(err, "Funds csv CLOSE")

		// VOTEPLANS
		votePlansFile, err := os.Create(filepath.Join(*dumpRaw, "sql_voteplans.csv"))
		kit.FatalOn(err, "Voteplans csv CREATE")
		vp := funds.First().VotePlans
		err = gocsv.MarshalFile(&vp, votePlansFile)
		kit.FatalOn(err, "Voteplans csv WRITE")
		err = votePlansFile.Close()
		kit.FatalOn(err, "Voteplans csv CLOSE")

		// PROPOSALS
		proposalsFile, err := os.Create(filepath.Join(*dumpRaw, "sql_proposals.csv"))
		kit.FatalOn(err, "Proposals csv CREATE")
		p := proposals.All()
		err = gocsv.MarshalFile(p, proposalsFile)
		kit.FatalOn(err, "Proposals csv WRITE")
		err = proposalsFile.Close()
		kit.FatalOn(err, "Proposals csv CLOSE")

		log.Printf("VIT - important data are dumped at (%s)", *dumpRaw)
		log.Println()
	}

	block0Yaml, err := block0cfg.ToYaml()
	kit.FatalOn(err)

	if *dumbGenesisDataPath != "" {
		bulkDumbData, err := ioutil.ReadFile(*dumbGenesisDataPath)
		kit.FatalOn(err)
		if len(bulkDumbData) > 0 {
			block0Yaml = append(block0Yaml, bulkDumbData...)
		}
	}

	// need this file for starting the node (--genesis-block)
	block0BinFile := filepath.Join(workingDir, "VIT-block0.bin")

	// keep also the text block0 config
	block0TxtFile := filepath.Join(workingDir, "VIT-block0.yaml")

	// block0BinFile will be created by jcli
	block0Bin, err := jcli.GenesisEncode(block0Yaml, "", block0BinFile)
	kit.FatalOn(err, kit.B2S(block0Bin))

	block0Hash, err := jcli.GenesisHash(block0Bin, "")
	kit.FatalOn(err, kit.B2S(block0Hash))

	// block0TxtFile will be created by jcli
	block0Txt, err := jcli.GenesisDecode(block0Bin, "", block0TxtFile)
	kit.FatalOn(err, kit.B2S(block0Txt))

	//////////////////////
	//  secrets config  //
	//////////////////////

	for i := range leaders {
		secretCfg := jnode.NewSecretConfig()

		secretCfg.Bft.SigningKey = leaders[i].sk

		secretCfgYaml, err := secretCfg.ToYaml()
		kit.FatalOn(err)

		// need this file for starting the node (--secret)
		secretCfgFile := filepath.Join(workingDir, strconv.Itoa(i)+"_bft-secret.yaml")
		err = ioutil.WriteFile(secretCfgFile, secretCfgYaml, 0744)
		kit.FatalOn(err)

		leaders[i].cfgFile = secretCfgFile
	}

	///////////////////
	//  node config  //
	///////////////////

	nodeCfg := jnode.NewNodeConfig()

	nodeCfg.Storage = filepath.Join(workingDir, "storage")
	nodeCfg.SkipBootstrap = *skipBootstrap
	nodeCfg.Rest.Listen = restAddress
	nodeCfg.Rest.Cors.AllowedOrigins = strings.Split(*restCorsAllowed, ",")
	nodeCfg.Rest.Cors.MaxAgeSecs = 0
	nodeCfg.P2P.PublicAddress = p2pListenAddress
	nodeCfg.P2P.ListenAddress = p2pListenAddress
	nodeCfg.P2P.AllowPrivateAddresses = true
	nodeCfg.BootstrapFromTrustedPeers = true
	nodeCfg.P2P.MaxBootstrapAttempts = 5
	nodeCfg.Log.Level = nodeCfgLogLevel

	nodeCfg.Explorer.Enabled = *explorerEnabled

	for i := range leaders {
		nodeCfg.AddSecretFile(leaders[i].cfgFile)
	}

	nodeCfgYaml, err := nodeCfg.ToYaml()
	kit.FatalOn(err)

	// need this file for starting the node (--config)
	nodeCfgFile := filepath.Join(workingDir, "node-config.yaml")
	err = ioutil.WriteFile(nodeCfgFile, nodeCfgYaml, 0644)
	kit.FatalOn(err)

	//////////////////////
	// running the node //
	//////////////////////

	// Check for jörmungandr binary. Local folder first, then PATH
	jnodeBin, err := kit.FindExecutable("jormungandr", "jor_bins")
	kit.FatalOn(err, jnodeBin)
	jnode.BinName(jnodeBin)

	// get jörmungandr version
	jormungandrVersion, err := jnode.VersionFull()
	kit.FatalOn(err, kit.B2S(jormungandrVersion))

	node := jnode.NewJnode()

	node.WorkingDir = workingDir
	node.GenesisBlock = block0BinFile
	node.ConfigFile = nodeCfgFile

	for i := range leaders {
		node.AddSecretFile(leaders[i].cfgFile)
	}

	// Run the node (Start + Wait)
	if *startNode {
		node.Stdout, err = os.Create(filepath.Join(workingDir, "stdout.log"))
		kit.FatalOn(err)
		node.Stderr, err = os.Create(filepath.Join(workingDir, "stderr.log"))
		kit.FatalOn(err)

		err = os.Setenv("RUST_BACKTRACE", "full")
		kit.FatalOn(err, "Failed to set env (RUST_BACKTRACE=full)")

		err = node.Run()
		if err != nil {
			log.Fatalf("node.Run FAILED: %v", err)
		}
	}

	////////////////////
	// internal proxy //
	////////////////////
	go func() {
		err := webproxy.Run(proposals, funds, &block0Bin, proxyAddress, "http://"+restAddress)
		if err != nil {
			kit.FatalOn(err, "Proxy Run")
		}
	}()

	log.Println()
	log.Printf("OS: %s, ARCH: %s", runtime.GOOS, runtime.GOARCH)
	log.Println()
	log.Printf("jcli: %s", jcliBin)
	log.Printf("ver : %s", jcliVersion)
	log.Println()
	log.Printf("node: %s", jnodeBin)
	log.Printf("ver : %s", jormungandrVersion)
	log.Println()
	log.Printf("VIT - BFT Genesis Hash: %s\n", kit.B2S(block0Hash))
	log.Println()
	log.Printf("VIT - BFT Genesis: %s - %d", "COMMITTEE", len(block0cfg.BlockchainConfiguration.Committees)+len(block0cfg.BlockchainConfiguration.ConsensusLeaderIds))
	log.Printf("VIT - BFT Genesis: %s - %d", "VOTEPLANS", len(jcliVotePlans))
	log.Printf("VIT - BFT Genesis: %s - %d", "PROPOSALS", proposals.Total())
	log.Println()
	log.Printf("VIT - BFT Genesis: %s", "Wallets available for recovery")

	qrPrint(wallets)

	log.Println()
	log.Printf("JÖRMUNGANDR listening at: %s - %v", p2pListenAddress, *startNode)
	log.Printf("JÖRMUNGANDR Rest API available at: http://%s/api - %v", restAddress, *startNode)
	log.Println()
	log.Printf("APP - PROXY Rest API available at: http://%s/api", proxyAddress)
	log.Println()
	log.Println("VIT - BFT Genesis Node - Running...")
	log.Println()

	if *startNode {
		node.Wait() // Wait for the node to stop.
	}

	if *allowNodeRestart || !*startNode {
		switch {
		case !*startNode:
			log.Println("The node has to be started manually or issue SIGINT/SIGTERM again.")
		case *allowNodeRestart:
			log.Println("The node has stopped. Please start the node manually and keep the same running config or issue SIGINT/SIGTERM again.")
		}

		log.Printf("%s %s", jnodeBin, strings.Join(node.BuildCmdArg(), " "))
		log.Println()

		// Listen for the service syscalls
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs

		if *shutdownNode {
			// Attempt node shutdown in case the node was restarted manually again
			_, _ = jcli.RestShutdown("http://"+restAddress+"/api", "")
		}
	}

	log.Println("...VIT - BFT Genesis Node - Done") // All done. Node has stopped.
}

// Print Wallet data and QR
func qrPrint(w []wallet.Wallet) {
	for i := range wallets {
		q, err := qrcode.New(w[i].Mnemonics, qrcode.Medium)
		kit.FatalOn(err)

		fmt.Printf("\n%s\n%s\n", w[i], q.ToSmallString(false))
	}
}

func votePlansNeeded(proposalsTot int, max int) int {
	votePlansNeeded, more := proposalsTot/max, proposalsTot%max
	if more > 0 {
		votePlansNeeded = votePlansNeeded + 1
	}
	return votePlansNeeded
}
