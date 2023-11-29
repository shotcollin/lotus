package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gin-contrib/pprof"
	"github.com/gin-gonic/gin"
	ds "github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/urfave/cli/v2"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-jsonrpc/auth"
	"github.com/filecoin-project/go-statestore"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/build"
	lcli "github.com/filecoin-project/lotus/cli"
	cliutil "github.com/filecoin-project/lotus/cli/util"
	"github.com/filecoin-project/lotus/journal"
	"github.com/filecoin-project/lotus/journal/alerting"
	"github.com/filecoin-project/lotus/journal/fsjournal"
	"github.com/filecoin-project/lotus/lib/harmony/harmonydb"
	"github.com/filecoin-project/lotus/lib/harmony/harmonytask"
	"github.com/filecoin-project/lotus/lib/ulimit"
	"github.com/filecoin-project/lotus/metrics"
	"github.com/filecoin-project/lotus/node"
	"github.com/filecoin-project/lotus/node/config"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/filecoin-project/lotus/node/repo"
	"github.com/filecoin-project/lotus/provider"
	"github.com/filecoin-project/lotus/provider/lpwinning"
	"github.com/filecoin-project/lotus/storage/ctladdr"
	"github.com/filecoin-project/lotus/storage/paths"
	"github.com/filecoin-project/lotus/storage/sealer"
	"github.com/filecoin-project/lotus/storage/sealer/ffiwrapper"
	"github.com/filecoin-project/lotus/storage/sealer/storiface"
)

type stackTracer interface {
	StackTrace() errors.StackTrace
}

var runCmd = &cli.Command{
	Name:  "run",
	Usage: "Start a lotus provider process",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "listen",
			Usage:   "host address and port the worker api will listen on",
			Value:   "0.0.0.0:12300",
			EnvVars: []string{"LOTUS_WORKER_LISTEN"},
		},
		&cli.BoolFlag{
			Name:  "nosync",
			Usage: "don't check full-node sync status",
		},
		&cli.BoolFlag{
			Name:   "halt-after-init",
			Usage:  "only run init, then return",
			Hidden: true,
		},
		&cli.BoolFlag{
			Name:  "manage-fdlimit",
			Usage: "manage open file limit",
			Value: true,
		},
		&cli.StringSliceFlag{
			Name:  "layers",
			Usage: "list of layers to be interpreted (atop defaults). Default: base",
			Value: cli.NewStringSlice("base"),
		},
		&cli.StringFlag{
			Name:  "storage-json",
			Usage: "path to json file containing storage config",
			Value: "~/.lotus-provider/storage.json",
		},
		&cli.StringFlag{
			Name:  "journal",
			Usage: "path to journal files",
			Value: "~/.lotus-provider/",
		},
	},
	Action: func(cctx *cli.Context) (err error) {
		defer func() {
			if err != nil {
				if err, ok := err.(stackTracer); ok {
					for _, f := range err.StackTrace() {
						fmt.Printf("%+s:%d\n", f, f)
					}
				}
			}
		}()
		if !cctx.Bool("enable-gpu-proving") {
			err := os.Setenv("BELLMAN_NO_GPU", "true")
			if err != nil {
				return err
			}
		}

		ctx, _ := tag.New(lcli.DaemonContext(cctx),
			tag.Insert(metrics.Version, build.BuildVersion),
			tag.Insert(metrics.Commit, build.CurrentCommit),
			tag.Insert(metrics.NodeType, "provider"),
		)
		shutdownChan := make(chan struct{})
		ctx, ctxclose := context.WithCancel(ctx)
		go func() {
			<-shutdownChan
			ctxclose()
		}()
		// Register all metric views
		/*
			if err := view.Register(
				metrics.MinerNodeViews...,
			); err != nil {
				log.Fatalf("Cannot register the view: %v", err)
			}
		*/
		// Set the metric to one so it is published to the exporter
		stats.Record(ctx, metrics.LotusInfo.M(1))

		if cctx.Bool("manage-fdlimit") {
			if _, _, err := ulimit.ManageFdLimit(); err != nil {
				log.Errorf("setting file descriptor limit: %s", err)
			}
		}

		deps, err := getDeps(ctx, cctx)

		if err != nil {
			return err
		}
		cfg, db, full, verif, lw, as, maddrs, stor, si, localStore := deps.cfg, deps.db, deps.full, deps.verif, deps.lw, deps.as, deps.maddrs, deps.stor, deps.si, deps.localStore

		///////////////////////////////////////////////////////////////////////
		///// Task Selection
		///////////////////////////////////////////////////////////////////////
		var activeTasks []harmonytask.TaskInterface
		{

			if cfg.Subsystems.EnableWindowPost {
				wdPostTask, wdPoStSubmitTask, derlareRecoverTask, err := provider.WindowPostScheduler(ctx, cfg.Fees, cfg.Proving, full, verif, lw,
					as, maddrs, db, stor, si, cfg.Subsystems.WindowPostMaxTasks)
				if err != nil {
					return err
				}
				activeTasks = append(activeTasks, wdPostTask, wdPoStSubmitTask, derlareRecoverTask)
			}

			if cfg.Subsystems.EnableWinningPost {
				winPoStTask := lpwinning.NewWinPostTask(cfg.Subsystems.WinningPostMaxTasks, db, lw, verif, full, maddrs)
				activeTasks = append(activeTasks, winPoStTask)
			}
		}
		log.Infow("This lotus_provider instance handles",
			"miner_addresses", maddrs,
			"tasks", lo.Map(activeTasks, func(t harmonytask.TaskInterface, _ int) string { return t.TypeDetails().Name }))

		taskEngine, err := harmonytask.New(db, activeTasks, deps.listenAddr)
		if err != nil {
			return err
		}

		gin.SetMode(gin.ReleaseMode)
		handler := gin.New()

		defer taskEngine.GracefullyTerminate(time.Hour)

		fh := &paths.FetchHandler{Local: localStore, PfHandler: &paths.DefaultPartialFileHandler{}}
		handler.NoRoute(gin.HandlerFunc(func(c *gin.Context) {
			if !auth.HasPerm(c, nil, api.PermAdmin) {
				c.JSON(401, struct{ Error string }{"unauthorized: missing admin permission"})
				return
			}

			fh.ServeHTTP(c.Writer, c.Request)
		}))
		// local APIs
		{
			// debugging
			handler.GET("/debug/metrics", gin.WrapH(metrics.Exporter()))
			pprof.Register(handler)
		}

		// Serve the RPC.
		/*
			endpoint, err := r.APIEndpoint()
			fmt.Println("Endpoint: ", endpoint)
			if err != nil {
				return fmt.Errorf("getting API endpoint: %w", err)
			}
			rpcStopper, err := node.ServeRPC(handler, "lotus-provider", endpoint)
			if err != nil {
				return fmt.Errorf("failed to start json-rpc endpoint: %s", err)
			}
		*/

		// Monitor for shutdown.
		// TODO provide a graceful shutdown API on shutdownChan
		finishCh := node.MonitorShutdown(shutdownChan) //node.ShutdownHandler{Component: "rpc server", StopFunc: rpcStopper},
		//node.ShutdownHandler{Component: "provider", StopFunc: stop},

		<-finishCh
		return nil
	},
}

func makeDB(cctx *cli.Context) (*harmonydb.DB, error) {
	dbConfig := config.HarmonyDB{
		Username: cctx.String("db-user"),
		Password: cctx.String("db-password"),
		Hosts:    strings.Split(cctx.String("db-host"), ","),
		Database: cctx.String("db-name"),
		Port:     cctx.String("db-port"),
	}
	return harmonydb.NewFromConfig(dbConfig)
}

type jwtPayload struct {
	Allow []auth.Permission
}

func StorageAuth(apiKey string) (sealer.StorageAuth, error) {
	if apiKey == "" {
		return nil, xerrors.Errorf("no api key provided")
	}

	rawKey, err := base64.StdEncoding.DecodeString(apiKey)
	if err != nil {
		return nil, xerrors.Errorf("decoding api key: %w", err)
	}

	key := jwt.NewHS256(rawKey)

	p := jwtPayload{
		Allow: []auth.Permission{"admin"},
	}

	token, err := jwt.Sign(&p, key)
	if err != nil {
		return nil, err
	}

	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+string(token))
	return sealer.StorageAuth(headers), nil
}

type Deps struct {
	cfg        *config.LotusProviderConfig
	db         *harmonydb.DB
	full       api.FullNode
	verif      storiface.Verifier
	lw         *sealer.LocalWorker
	as         *ctladdr.AddressSelector
	maddrs     []dtypes.MinerAddress
	stor       *paths.Remote
	si         *paths.DBIndex
	localStore *paths.Local
	listenAddr string
}

func getDeps(ctx context.Context, cctx *cli.Context) (*Deps, error) {
	// Open repo

	repoPath := cctx.String(FlagRepoPath)
	fmt.Println("repopath", repoPath)
	r, err := repo.NewFS(repoPath)
	if err != nil {
		return nil, err
	}

	ok, err := r.Exists()
	if err != nil {
		return nil, err
	}
	if !ok {
		if err := r.Init(repo.Provider); err != nil {
			return nil, err
		}
	}

	db, err := makeDB(cctx)
	if err != nil {
		return nil, err
	}

	///////////////////////////////////////////////////////////////////////
	///// Dependency Setup
	///////////////////////////////////////////////////////////////////////

	// The config feeds into task runners & their helpers
	cfg, err := getConfig(cctx, db)
	if err != nil {
		return nil, err
	}

	log.Debugw("config", "config", cfg)

	var verif storiface.Verifier = ffiwrapper.ProofVerifier

	as, err := provider.AddressSelector(&cfg.Addresses)()
	if err != nil {
		return nil, err
	}

	de, err := journal.ParseDisabledEvents(cfg.Journal.DisabledEvents)
	if err != nil {
		return nil, err
	}
	j, err := fsjournal.OpenFSJournalPath(cctx.String("journal"), de)
	if err != nil {
		return nil, err
	}

	full, fullCloser, err := cliutil.GetFullNodeAPIV1LotusProvider(cctx, cfg.Apis.ChainApiInfo)
	if err != nil {
		return nil, err
	}

	go func() {
		select {
		case <-ctx.Done():
			fullCloser()
			_ = j.Close()
		}
	}()
	sa, err := StorageAuth(cfg.Apis.StorageRPCSecret)
	if err != nil {
		return nil, xerrors.Errorf(`'%w' while parsing the config toml's 
	[Apis]
	StorageRPCSecret=%v
Get it with: jq .PrivateKey ~/.lotus-miner/keystore/MF2XI2BNNJ3XILLQOJUXMYLUMU`, err, cfg.Apis.StorageRPCSecret)
	}

	al := alerting.NewAlertingSystem(j)
	si := paths.NewDBIndex(al, db)
	bls := &paths.BasicLocalStorage{
		PathToJSON: cctx.String("storage-json"),
	}

	listenAddr := cctx.String("listen")
	const unspecifiedAddress = "0.0.0.0"
	addressSlice := strings.Split(listenAddr, ":")
	if ip := net.ParseIP(addressSlice[0]); ip != nil {
		if ip.String() == unspecifiedAddress {
			rip, err := db.GetRoutableIP()
			if err != nil {
				return nil, err
			}
			listenAddr = rip + ":" + addressSlice[1]
		}
	}
	localStore, err := paths.NewLocal(ctx, bls, si, []string{"http://" + listenAddr + "/remote"})
	if err != nil {
		return nil, err
	}

	stor := paths.NewRemote(localStore, si, http.Header(sa), 10, &paths.DefaultPartialFileHandler{})

	wstates := statestore.New(dssync.MutexWrap(ds.NewMapDatastore()))

	// todo localWorker isn't the abstraction layer we want to use here, we probably want to go straight to ffiwrapper
	//  maybe with a lotus-provider specific abstraction. LocalWorker does persistent call tracking which we probably
	//  don't need (ehh.. maybe we do, the async callback system may actually work decently well with harmonytask)
	lw := sealer.NewLocalWorker(sealer.WorkerConfig{}, stor, localStore, si, nil, wstates)

	var maddrs []dtypes.MinerAddress
	for _, s := range cfg.Addresses.MinerAddresses {
		addr, err := address.NewFromString(s)
		if err != nil {
			return nil, err
		}
		maddrs = append(maddrs, dtypes.MinerAddress(addr))
	}

	return &Deps{ // lint: intentionally not-named so it will fail if one is forgotten
		cfg,
		db,
		full,
		verif,
		lw,
		as,
		maddrs,
		stor,
		si,
		localStore,
		listenAddr,
	}, nil

}