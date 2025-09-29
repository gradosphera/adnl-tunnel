package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/natefinch/lumberjack"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/ton-blockchain/adnl-tunnel/config"
	"github.com/ton-blockchain/adnl-tunnel/metrics"
	"github.com/ton-blockchain/adnl-tunnel/tunnel"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/ton-payment-network/tonpayments/chain"
	chainClient "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	adnlTransport "github.com/xssnick/ton-payment-network/tonpayments/transport/adnl"
	pWallet "github.com/xssnick/ton-payment-network/tonpayments/wallet"
	tonaddr "github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/address"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strings"
	"time"

	_ "net/http/pprof"
)

var ConfigPath = flag.String("config", "config.json", "Config path")
var PaymentNodeWith = flag.String("payment-node", "", "Payment node to open channel with")
var Verbosity = flag.Int("v", 2, "verbosity")
var GenerateSharedExample = flag.String("gen-shared-config", "", "Will generate shared config file with current node, at specified path")

var LogFilename = flag.String("log-filename", "tunnel.log", "log file name")
var LogMaxSize = flag.Int("log-max-size", 1024, "maximum log file size in MB before rotation")
var LogMaxBackups = flag.Int("log-max-backups", 16, "maximum number of old log files to keep")
var LogMaxAge = flag.Int("log-max-age", 180, "maximum number of days to retain old log files")
var ProfileAddr = flag.String("profile-listen-addr", "", "Addr to run the pprof server on (optional, disabled if empty)")
var MetricsAddr = flag.String("metrics-listen-addr", "", "Addr to run the prometheus metrics server on (optional, disabled if empty)")
var LogCompress = flag.Bool("log-compress", false, "whether to compress rotated log files")
var LogDisableFile = flag.Bool("log-disable-file", false, "Disable logging to file")

var GitCommit = "dev"

func main() {
	flag.Parse()

	// logs rotation
	var logWriters = []io.Writer{zerolog.NewConsoleWriter()}

	if !*LogDisableFile {
		logWriters = append(logWriters, &lumberjack.Logger{
			Filename:   *LogFilename,
			MaxSize:    *LogMaxSize, // mb
			MaxBackups: *LogMaxBackups,
			MaxAge:     *LogMaxAge, // days
			Compress:   *LogCompress,
		})
	}
	multi := zerolog.MultiLevelWriter(logWriters...)

	log.Logger = zerolog.New(multi).With().Timestamp().Logger().Level(zerolog.InfoLevel)
	adnl.Logger = func(v ...any) {}

	log.Info().Str("version", GitCommit).Msg("starting tunnel node...")

	if *Verbosity >= 5 {
		dht.Logger = func(v ...any) {
			log.Logger.Debug().Msg(fmt.Sprintln(v...))
		}
	}

	if *Verbosity >= 6 {
		adnl.Logger = func(v ...any) {
			log.Logger.Debug().Msg(fmt.Sprintln(v...))
		}
	}

	if *ProfileAddr != "" {
		go func() {
			runtime.SetMutexProfileFraction(1)
			log.Info().Str("addr", *ProfileAddr).Msg("starting pprof server")
			if err := http.ListenAndServe(*ProfileAddr, nil); err != nil {
				log.Fatal().Err(err).Msg("error starting pprof server")
			}
		}()
	}

	if *MetricsAddr != "" {
		metrics.RegisterMetrics()
		go func() {
			log.Info().Str("addr", *MetricsAddr).Msg("starting metrics server")
			if err := http.ListenAndServe(*MetricsAddr, promhttp.Handler()); err != nil {
				log.Fatal().Err(err).Msg("error starting metrics server")
			}
		}()
	}

	if *Verbosity >= 3 {
		log.Logger = log.Logger.Level(zerolog.DebugLevel).With().Logger()
	} else if *Verbosity == 2 {
		log.Logger = log.Logger.Level(zerolog.InfoLevel).With().Logger()
	} else if *Verbosity == 1 {
		log.Logger = log.Logger.Level(zerolog.WarnLevel).With().Logger()
	} else if *Verbosity == 0 {
		log.Logger = log.Logger.Level(zerolog.ErrorLevel).With().Logger()
	} else {
		log.Logger = log.Logger.Level(zerolog.FatalLevel).With().Logger()
	}

	cfg, err := config.LoadConfig(*ConfigPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
		return
	}

	if *GenerateSharedExample != "" {
		if !strings.HasSuffix(*GenerateSharedExample, ".json") {
			log.Fatal().Msg("shared config path must end with .json")
			return
		}

		if _, err = config.GenerateSharedConfig(cfg, *GenerateSharedExample); err != nil {
			log.Fatal().Err(err).Msg("failed to generate shared config")
			return
		}

		log.Info().Str("path", *GenerateSharedExample).Msg("shared config generated")
		return
	}

	_, dhtKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to generate DHT key")
		return
	}

	threads := int(cfg.TunnelThreads)
	if threads == 0 {
		threads = runtime.NumCPU()
	}

	listenAddr, err := netip.ParseAddrPort(cfg.TunnelListenAddr)
	if err != nil {
		log.Fatal().Err(err).Msg("Invalid listen address")
		return
	}

	tunKey := ed25519.NewKeyFromSeed(cfg.TunnelServerKey)
	gate := adnl.NewGateway(tunKey)
	if cfg.ExternalIP != "" {
		ip := net.ParseIP(cfg.ExternalIP)
		if ip == nil {
			log.Fatal().Msg("Invalid external IP address")
			return
		}
		gate.SetAddressList([]*address.UDP{
			{
				IP:   ip.To4(),
				Port: int32(listenAddr.Port()),
			},
		})
	}

	if err = gate.StartServer(cfg.TunnelListenAddr, threads); err != nil {
		log.Fatal().Err(err).Msg("start gateway as server failed")
		return
	}

	dhtGate := adnl.NewGateway(dhtKey)
	if err = dhtGate.StartClient(); err != nil {
		log.Fatal().Err(err).Msg("start dht gateway failed")
		return
	}

	gCfg, err := liteclient.GetConfigFromUrl(context.Background(), cfg.NetworkConfigUrl)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to get global config")
	}

	dhtClient, err := dht.NewClientFromConfig(dhtGate, gCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create DHT client")
		return
	}

	var wlt *wallet.Wallet
	var pmt tunnel.PaymentConfig
	var apiClient ton.APIClientWrapped
	if cfg.PaymentsEnabled {
		log.Info().Msg("Initializing payment node ")
		pm, w, apiC := preparePayments(context.Background(), gCfg, dhtClient, cfg)
		go pm.Start()

		var ch []byte
		if *PaymentNodeWith != "" {
			ch, err = hex.DecodeString(*PaymentNodeWith)
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to parse payment node key")
				return
			}
			if len(ch) != ed25519.PublicKeySize {
				log.Fatal().Msg("Invalid payment node key size")
				return
			}
		}

		chId, err := preparePaymentChannel(context.Background(), pm, ch)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to prepare payment channels")
		}
		log.Info().Str("payment-pub-key", base64.StdEncoding.EncodeToString(chId)).Msg("prioritized channel for payments is active")

		pmt = tunnel.PaymentConfig{
			Service:                pm,
			MinPricePerPacketRoute: cfg.Payments.MinPricePerPacketRoute,
			MinPricePerPacketInOut: cfg.Payments.MinPricePerPacketInOut,
		}
		wlt = w
		apiClient = apiC
	}

	lvl := zerolog.InfoLevel
	if *Verbosity >= 3 {
		lvl = zerolog.DebugLevel
	}
	tGate := tunnel.NewGateway(gate, dhtClient, tunKey, log.With().Str("component", "gateway").Logger().Level(lvl), pmt)
	go func() {
		if err = tGate.Start(); err != nil {
			log.Fatal().Err(err).Msg("tunnel gateway failed")
			return
		}
	}()

	speedPrinterCtx, cancelSp := context.WithCancel(context.Background())
	cancelSp()

	log.Info().Msg("Tunnel started, listening on " + cfg.TunnelListenAddr + " ADNL id is: " + base64.StdEncoding.EncodeToString(tunKey.Public().(ed25519.PublicKey)))
	for {
		log.Info().Msg("Input a command:")
		var val string
		if _, err = fmt.Scanln(&val); err != nil {
			log.Error().Err(err).Msg("input failure")
			time.Sleep(100 * time.Millisecond)
			continue
		}

		switch val {
		case "speed":
			select {
			case <-speedPrinterCtx.Done():
				speedPrinterCtx, cancelSp = context.WithCancel(context.Background())

				go func() {
					prev := tGate.GetPacketsStats()
					for {
						select {
						case <-speedPrinterCtx.Done():
							return
						case <-time.After(time.Second * 1):
							stats := tGate.GetPacketsStats()
							for s, st := range stats {
								if p := prev[s]; p != nil {
									log.Info().Hex("section", []byte(s)).
										Str("routed", formatNum(st.Routed-p.Routed)+"/s").
										Str("sent", formatNum(st.Sent-p.Sent)+"/s").
										Str("received", formatNum(st.Received-p.Received)+"/s").
										Msg("per second")
								}
							}
							prev = stats
						}
					}
				}()

			default:
				cancelSp()
			}
		case "stats":
			stats := tGate.GetPacketsStats()
			for s, st := range stats {
				log.Info().Hex("section", []byte(s)).
					Str("routed", formatNum(st.Routed)).
					Str("sent", formatNum(st.Sent)).
					Str("received", formatNum(st.Received)).
					Ints64("prepaid_routes", st.PrepaidPacketsRoute).
					Str("prepaid_out", formatNumInt(st.PrepaidPacketsOut)).
					Str("prepaid_in", formatNumInt(st.PrepaidPacketsIn)).
					Msg("stats summarized")
			}
		case "balance", "capacity":
			if pmt.Service == nil {
				log.Error().Msg("payments are not enabled")
				continue
			}

			list, err := pmt.Service.ListChannels(context.Background(), nil, db.ChannelStateActive)
			if err != nil {
				log.Error().Err(err).Msg("Failed to list channels")
				continue
			}

			amount := big.NewInt(0)
			for _, channel := range list {
				v, _, err := channel.CalcBalance(val == "capacity")
				if err != nil {
					log.Error().Err(err).Msg("Failed to calc channel balance")
					continue
				}
				amount = amount.Add(amount, v)
			}

			if val == "balance" {
				log.Info().Msg("Summarized balance: " + tlb.FromNanoTON(amount).String() + " TON")
			} else {
				log.Info().Msg("Capacity left: " + tlb.FromNanoTON(amount).String() + " TON")
			}
			continue
		case "wallet-ton-balance":
			if wlt == nil {
				log.Error().Msg("payments are not enabled")
				continue
			}

			blk, err := apiClient.CurrentMasterchainInfo(context.Background())
			if err != nil {
				log.Error().Err(err).Msg("failed to get current masterchain info")
				continue
			}

			balance, err := wlt.GetBalance(context.Background(), blk)
			if err != nil {
				log.Error().Err(err).Msg("failed to get balance")
				continue
			}

			log.Info().Msgf("wallet balance: %s TON", balance.String())
		case "wallet-ton-transfer":
			if wlt == nil {
				log.Error().Msg("payments are not enabled")
				continue
			}

			log.Info().Msg("enter address to transfer to:")

			var addrStr string
			_, _ = fmt.Scanln(&addrStr)

			addr, err := tonaddr.ParseAddr(addrStr)
			if err != nil {
				log.Error().Err(err).Msg("incorrect format of address")
				continue
			}

			log.Info().Msg("input amount:")
			var strAmt string
			_, _ = fmt.Scanln(&strAmt)

			amt, err := tlb.FromTON(strAmt)
			if err != nil {
				log.Error().Err(err).Msg("incorrect format of amount")
				continue
			}

			log.Info().Msg("input comment:")
			var comment string
			_, _ = fmt.Scanln(&comment)

			log.Info().
				Str("to_address", addr.String()).
				Str("amount", amt.String()).
				Msg("transferring...")

			time.Sleep(3 * time.Second) // give user some time to cancel

			pmt.Service.GetPrivateKey()

			tx, _, err := wlt.TransferWaitTransaction(context.Background(), addr, amt, comment)
			if err != nil {
				log.Error().Err(err).Msg("failed to transfer")
				continue
			}
			log.Info().Str("hash", base64.URLEncoding.EncodeToString(tx.Hash)).Msg("transfer transaction committed")
		}

	}
}

func formatNum(packets uint64) string {
	sizes := []string{"", " K", " M", " B"}

	sizeIndex := 0
	sizeFloat := float64(packets)

	for sizeFloat >= 1000 && sizeIndex < len(sizes)-1 {
		sizeFloat /= 1000
		sizeIndex++
	}

	return fmt.Sprintf("%.2f%s", sizeFloat, sizes[sizeIndex])
}

func formatNumInt(packets int64) string {
	sizes := []string{"", " K", " M", " B"}

	sizeIndex := 0
	sizeFloat := float64(packets)

	for sizeFloat >= 1000 && sizeIndex < len(sizes)-1 {
		sizeFloat /= 1000
		sizeIndex++
	}

	return fmt.Sprintf("%.2f%s", sizeFloat, sizes[sizeIndex])
}

func preparePayments(ctx context.Context, gCfg *liteclient.GlobalConfig, dhtClient *dht.Client, cfg *config.Config) (*tonpayments.Service, *wallet.Wallet, ton.APIClientWrapped) {
	client := liteclient.NewConnectionPool()

	log.Info().Msg("initializing ton client with verified proof chain...")

	// connect to lite servers
	if err := client.AddConnectionsFromConfig(ctx, gCfg); err != nil {
		log.Fatal().Err(err).Msg("ton connect err")
		return nil, nil, nil
	}

	policy := ton.ProofCheckPolicyFast
	if cfg.Payments.SecureProofPolicy {
		policy = ton.ProofCheckPolicySecure
	}

	// initialize ton api lite connection wrapper
	apiClient := ton.NewAPIClient(client, policy).WithRetry(2).WithTimeout(5 * time.Second)
	if cfg.Payments.SecureProofPolicy {
		apiClient.SetTrustedBlockFromConfig(gCfg)
	}

	nodePrv := ed25519.NewKeyFromSeed(cfg.Payments.PaymentsNodeKey)
	serverPrv := ed25519.NewKeyFromSeed(cfg.Payments.ADNLServerKey)
	gate := adnl.NewGateway(serverPrv)

	if err := gate.StartClient(); err != nil {
		log.Fatal().Err(err).Msg("failed to init adnl payments gateway")
		return nil, nil, nil
	}

	ldb, freshDb, err := leveldb.NewLevelDB(cfg.Payments.DBPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init leveldb")
		return nil, nil, nil
	}

	walletPrv := ed25519.NewKeyFromSeed(cfg.Payments.WalletPrivateKey)
	fdb := db.NewDB(ldb, nodePrv.Public().(ed25519.PublicKey))

	if freshDb {
		if err = fdb.SetMigrationVersion(context.Background(), len(db.Migrations)); err != nil {
			log.Fatal().Err(err).Msg("failed to set initial migration version")
			return nil, nil, nil
		}
	} else {
		if err = db.RunMigrations(fdb); err != nil {
			log.Fatal().Err(err).Msg("failed to run migrations")
			return nil, nil, nil
		}
	}

	srv := adnlTransport.NewServer(dhtClient, gate, serverPrv, nodePrv, cfg.ExternalIP != "")
	tr := transport.NewTransport(nodePrv, srv, false)

	var seqno uint32
	if bo, err := fdb.GetBlockOffset(ctx); err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			log.Fatal().Err(err).Msg("failed to load block offset")
			return nil, nil, nil
		}
	} else {
		seqno = bo.Seqno
	}

	scanLog := log.Logger
	if *Verbosity >= 4 {
		scanLog = scanLog.Level(zerolog.DebugLevel).With().Logger()
	}

	inv := make(chan any)
	sc := chain.NewScanner(apiClient, seqno, scanLog)
	if err = sc.StartSmall(inv); err != nil {
		log.Fatal().Err(err).Msg("failed to start scanner")
		return nil, nil, nil
	}
	fdb.SetOnChannelUpdated(sc.OnChannelUpdate)

	chList, err := fdb.GetChannels(context.Background(), nil, db.ChannelStateAny)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load channels")
		return nil, nil, nil
	}

	for _, channel := range chList {
		if channel.Status != db.ChannelStateInactive {
			sc.OnChannelUpdate(context.Background(), channel, true)
		}
	}

	w, err := pWallet.InitWallet(apiClient, walletPrv)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init wallet")
		return nil, nil, nil
	}
	log.Info().Str("addr", w.WalletAddress().String()).Msg("wallet initialized")

	svc, err := tonpayments.NewService(chainClient.NewTON(apiClient), fdb, tr, nil, w, inv, nodePrv, cfg.Payments.ChannelsConfig, *MetricsAddr != "")
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init payments service")
		return nil, nil, nil
	}
	tr.SetService(svc)
	log.Info().Str("pubkey", base64.StdEncoding.EncodeToString(nodePrv.Public().(ed25519.PublicKey))).Msg("payment node initialized")

	return svc, w.Wallet(), apiClient
}

func preparePaymentChannel(ctx context.Context, pmt *tonpayments.Service, ch []byte) ([]byte, error) {
	list, err := pmt.ListChannels(ctx, nil, db.ChannelStateActive)
	if err != nil {
		return nil, fmt.Errorf("failed to list channels: %w", err)
	}

	var best []byte
	var bestAmount = big.NewInt(0)
	for _, channel := range list {
		if len(ch) > 0 {
			if bytes.Equal(channel.TheirOnchain.Key, ch) {
				// we have specified channel already deployed
				return channel.TheirOnchain.Key, nil
			}
			continue
		}

		// if specific channel not defined we select the channel with the biggest deposit
		if channel.TheirOnchain.Deposited.Cmp(bestAmount) >= 0 {
			bestAmount = channel.TheirOnchain.Deposited
			best = channel.TheirOnchain.Key
		}
	}

	if best != nil {
		return best, nil
	}

	var inp string

	// if no channels (or specified channel) are nod deployed, we deploy
	if len(ch) == 0 {
		log.Warn().Msg("No active onchain payment channel found, please input payment node id (pub key) in base64 format, to deploy channel with:")
		if _, err = fmt.Scanln(&inp); err != nil {
			return nil, fmt.Errorf("failed to read input: %w", err)
		}

		ch, err = base64.StdEncoding.DecodeString(inp)
		if err != nil {
			return nil, fmt.Errorf("invalid id formet: %w", err)
		}
	}

	if len(ch) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid channel id length")
	}

	ctxTm, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	addr, err := pmt.OpenChannelWithNode(ctxTm, ch, nil, 0)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("failed to deploy channel with node: %w", err)
	}
	log.Info().Msg("onchain channel deployed at address: " + addr.String() + " waiting for states exchange...")

	for {
		channel, err := pmt.GetChannel(context.Background(), addr.String())
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			return nil, fmt.Errorf("failed to get channel: %w", err)
		}

		if !channel.Our.IsReady() || !channel.Their.IsReady() {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		break
	}
	log.Info().Str("address", addr.String()).Msg("Channel states exchange completed")

	return ch, nil
}
