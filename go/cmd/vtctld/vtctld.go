package main

import (
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/context"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/acl"
	"github.com/youtube/vitess/go/timer"
	"github.com/youtube/vitess/go/vt/schemamanager"
	"github.com/youtube/vitess/go/vt/servenv"
	"github.com/youtube/vitess/go/vt/tabletmanager/tmclient"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/topotools"
	"github.com/youtube/vitess/go/vt/wrangler"
)

var (
	templateDir               = flag.String("templates", "", "directory containing templates")
	debug                     = flag.Bool("debug", false, "recompile templates for every request")
	schemaChangeDir           = flag.String("schema-change-dir", "", "directory contains schema changes for all keyspaces. Each keyspace has its own directory and schema changes are expected to live in '$KEYSPACE/input' dir. e.g. test_keyspace/input/*sql, each sql file represents a schema change")
	schemaChangeController    = flag.String("schema-change-controller", "", "schema change controller is responsible for finding schema changes and responsing schema change events")
	schemaChangeCheckInterval = flag.Int("schema-change-check-interval", 60, "this value decides how often we check schema change dir, in seconds")
	schemaChangeUser          = flag.String("schema-change-user", "", "The user who submits this schema change.")
)

func init() {
	servenv.RegisterDefaultFlags()
	servenv.InitServiceMapForBsonRpcService("vtctl")
}

func httpError(w http.ResponseWriter, format string, err error) {
	log.Errorf(format, err)
	http.Error(w, fmt.Sprintf(format, err), http.StatusInternalServerError)
}

// DbTopologyResult encapsulates a topotools.Topology and the possible error
type DbTopologyResult struct {
	Topology *topotools.Topology
	Error    string
}

// IndexContent has the list of toplevel links
type IndexContent struct {
	// maps a name to a linked URL
	ToplevelLinks map[string]string
}

// used at runtime by plug-ins
var templateLoader *TemplateLoader
var actionRepo *ActionRepository
var indexContent = IndexContent{
	ToplevelLinks: map[string]string{},
}

var ts topo.Server

func main() {
	flag.Parse()
	servenv.Init()
	defer servenv.Close()
	templateLoader = NewTemplateLoader(*templateDir, *debug)

	ts = topo.GetServer()
	defer topo.CloseServers()

	actionRepo = NewActionRepository(ts)

	// keyspace actions
	actionRepo.RegisterKeyspaceAction("ValidateKeyspace",
		func(ctx context.Context, wr *wrangler.Wrangler, keyspace string, r *http.Request) (string, error) {
			return "", wr.ValidateKeyspace(ctx, keyspace, false)
		})

	actionRepo.RegisterKeyspaceAction("ValidateSchemaKeyspace",
		func(ctx context.Context, wr *wrangler.Wrangler, keyspace string, r *http.Request) (string, error) {
			return "", wr.ValidateSchemaKeyspace(ctx, keyspace, nil, false)
		})

	actionRepo.RegisterKeyspaceAction("ValidateVersionKeyspace",
		func(ctx context.Context, wr *wrangler.Wrangler, keyspace string, r *http.Request) (string, error) {
			return "", wr.ValidateVersionKeyspace(ctx, keyspace)
		})

	actionRepo.RegisterKeyspaceAction("ValidatePermissionsKeyspace",
		func(ctx context.Context, wr *wrangler.Wrangler, keyspace string, r *http.Request) (string, error) {
			return "", wr.ValidatePermissionsKeyspace(ctx, keyspace)
		})

	// shard actions
	actionRepo.RegisterShardAction("ValidateShard",
		func(ctx context.Context, wr *wrangler.Wrangler, keyspace, shard string, r *http.Request) (string, error) {
			return "", wr.ValidateShard(ctx, keyspace, shard, false)
		})

	actionRepo.RegisterShardAction("ValidateSchemaShard",
		func(ctx context.Context, wr *wrangler.Wrangler, keyspace, shard string, r *http.Request) (string, error) {
			return "", wr.ValidateSchemaShard(ctx, keyspace, shard, nil, false)
		})

	actionRepo.RegisterShardAction("ValidateVersionShard",
		func(ctx context.Context, wr *wrangler.Wrangler, keyspace, shard string, r *http.Request) (string, error) {
			return "", wr.ValidateVersionShard(ctx, keyspace, shard)
		})

	actionRepo.RegisterShardAction("ValidatePermissionsShard",
		func(ctx context.Context, wr *wrangler.Wrangler, keyspace, shard string, r *http.Request) (string, error) {
			return "", wr.ValidatePermissionsShard(ctx, keyspace, shard)
		})

	// tablet actions
	actionRepo.RegisterTabletAction("Ping", "",
		func(ctx context.Context, wr *wrangler.Wrangler, tabletAlias topo.TabletAlias, r *http.Request) (string, error) {
			ti, err := wr.TopoServer().GetTablet(ctx, tabletAlias)
			if err != nil {
				return "", err
			}
			return "", wr.TabletManagerClient().Ping(ctx, ti)
		})

	actionRepo.RegisterTabletAction("ScrapTablet", acl.ADMIN,
		func(ctx context.Context, wr *wrangler.Wrangler, tabletAlias topo.TabletAlias, r *http.Request) (string, error) {
			// refuse to scrap tablets that are not spare
			ti, err := wr.TopoServer().GetTablet(ctx, tabletAlias)
			if err != nil {
				return "", err
			}
			if ti.Type != topo.TYPE_SPARE {
				return "", fmt.Errorf("Can only scrap spare tablets")
			}
			return "", wr.Scrap(ctx, tabletAlias, false, false)
		})

	actionRepo.RegisterTabletAction("ScrapTabletForce", acl.ADMIN,
		func(ctx context.Context, wr *wrangler.Wrangler, tabletAlias topo.TabletAlias, r *http.Request) (string, error) {
			// refuse to scrap tablets that are not spare
			ti, err := wr.TopoServer().GetTablet(ctx, tabletAlias)
			if err != nil {
				return "", err
			}
			if ti.Type != topo.TYPE_SPARE {
				return "", fmt.Errorf("Can only scrap spare tablets")
			}
			return "", wr.Scrap(ctx, tabletAlias, true, false)
		})

	actionRepo.RegisterTabletAction("DeleteTablet", acl.ADMIN,
		func(ctx context.Context, wr *wrangler.Wrangler, tabletAlias topo.TabletAlias, r *http.Request) (string, error) {
			return "", wr.DeleteTablet(ctx, tabletAlias)
		})

	// keyspace actions
	http.HandleFunc("/keyspace_actions", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		action := r.FormValue("action")
		if action == "" {
			http.Error(w, "no action provided", http.StatusBadRequest)
			return
		}

		keyspace := r.FormValue("keyspace")
		if keyspace == "" {
			http.Error(w, "no keyspace provided", http.StatusBadRequest)
			return
		}
		result := actionRepo.ApplyKeyspaceAction(action, keyspace, r)

		templateLoader.ServeTemplate("action.html", result, w, r)
	})

	// shard actions
	http.HandleFunc("/shard_actions", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		action := r.FormValue("action")
		if action == "" {
			http.Error(w, "no action provided", http.StatusBadRequest)
			return
		}

		keyspace := r.FormValue("keyspace")
		if keyspace == "" {
			http.Error(w, "no keyspace provided", http.StatusBadRequest)
			return
		}
		shard := r.FormValue("shard")
		if shard == "" {
			http.Error(w, "no shard provided", http.StatusBadRequest)
			return
		}
		result := actionRepo.ApplyShardAction(action, keyspace, shard, r)

		templateLoader.ServeTemplate("action.html", result, w, r)
	})

	// tablet actions
	http.HandleFunc("/tablet_actions", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		action := r.FormValue("action")
		if action == "" {
			http.Error(w, "no action provided", http.StatusBadRequest)
			return
		}

		alias := r.FormValue("alias")
		if alias == "" {
			http.Error(w, "no alias provided", http.StatusBadRequest)
			return
		}
		tabletAlias, err := topo.ParseTabletAliasString(alias)
		if err != nil {
			http.Error(w, "bad alias provided", http.StatusBadRequest)
			return
		}
		result := actionRepo.ApplyTabletAction(action, tabletAlias, r)

		templateLoader.ServeTemplate("action.html", result, w, r)
	})

	// topology server
	http.HandleFunc("/dbtopo", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		result := DbTopologyResult{}
		ctx := context.TODO()
		topology, err := topotools.DbTopology(ctx, ts)
		if err == nil && modifyDbTopology != nil {
			err = modifyDbTopology(ctx, ts, topology)
		}
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Topology = topology
		}
		templateLoader.ServeTemplate("dbtopo.html", result, w, r)
	})

	// serving graph
	http.HandleFunc("/serving_graph/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")

		cell := parts[len(parts)-1]
		if cell == "" {
			ctx := context.Background()
			cells, err := ts.GetKnownCells(ctx)
			if err != nil {
				httpError(w, "cannot get known cells: %v", err)
				return
			}
			templateLoader.ServeTemplate("serving_graph_cells.html", cells, w, r)
			return
		}

		ctx := context.Background()
		servingGraph := topotools.DbServingGraph(ctx, ts, cell)
		if modifyDbServingGraph != nil {
			modifyDbServingGraph(ctx, ts, servingGraph)
		}
		templateLoader.ServeTemplate("serving_graph.html", servingGraph, w, r)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		templateLoader.ServeTemplate("index.html", indexContent, w, r)
	})

	http.HandleFunc("/content/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, *templateDir+r.URL.Path[8:])
	})

	// vschema viewer
	http.HandleFunc("/vschema", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		schemafier, ok := ts.(topo.Schemafier)
		if !ok {
			httpError(w, "%s", fmt.Errorf("%T doesn's support schemafier API", ts))
		}
		var data struct {
			Error         error
			Input, Output string
		}
		ctx := context.Background()
		switch r.Method {
		case "POST":
			data.Input = r.FormValue("vschema")
			data.Error = schemafier.SaveVSchema(ctx, data.Input)
		}
		vschema, err := schemafier.GetVSchema(ctx)
		if err != nil {
			if data.Error == nil {
				data.Error = fmt.Errorf("Error fetching schema: %s", err)
			}
		}
		data.Output = vschema
		templateLoader.ServeTemplate("vschema.html", data, w, r)
	})

	// redirects for explorers
	http.HandleFunc("/explorers/redirect", func(w http.ResponseWriter, r *http.Request) {
		if explorer == nil {
			http.Error(w, "no explorer configured", http.StatusInternalServerError)
			return
		}
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}

		target, err := handleExplorerRedirect(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		http.Redirect(w, r, target, http.StatusFound)
	})

	// serve some data
	knownCellsCache := newKnownCellsCache(ts)
	http.HandleFunc("/json/KnownCells", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		result, err := knownCellsCache.Get(ctx)
		if err != nil {
			httpError(w, "error getting known cells: %v", err)
			return
		}
		w.Write(result)
	})

	keyspacesCache := newKeyspacesCache(ts)
	http.HandleFunc("/json/Keyspaces", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		result, err := keyspacesCache.Get(ctx)
		if err != nil {
			httpError(w, "error getting keyspaces: %v", err)
			return
		}
		w.Write(result)
	})

	keyspaceCache := newKeyspaceCache(ts)
	http.HandleFunc("/json/Keyspace", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		keyspace := r.FormValue("keyspace")
		if keyspace == "" {
			http.Error(w, "no keyspace provided", http.StatusBadRequest)
			return
		}
		ctx := context.Background()
		result, err := keyspaceCache.Get(ctx, keyspace)
		if err != nil {
			httpError(w, "error getting keyspace: %v", err)
			return
		}
		w.Write(result)
	})

	shardNamesCache := newShardNamesCache(ts)
	http.HandleFunc("/json/ShardNames", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		keyspace := r.FormValue("keyspace")
		if keyspace == "" {
			http.Error(w, "no keyspace provided", http.StatusBadRequest)
			return
		}
		ctx := context.Background()
		result, err := shardNamesCache.Get(ctx, keyspace)
		if err != nil {
			httpError(w, "error getting shardNames: %v", err)
			return
		}
		w.Write(result)
	})

	shardCache := newShardCache(ts)
	http.HandleFunc("/json/Shard", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		keyspace := r.FormValue("keyspace")
		if keyspace == "" {
			http.Error(w, "no keyspace provided", http.StatusBadRequest)
			return
		}
		shard := r.FormValue("shard")
		if shard == "" {
			http.Error(w, "no shard provided", http.StatusBadRequest)
			return
		}
		ctx := context.Background()
		result, err := shardCache.Get(ctx, keyspace+"/"+shard)
		if err != nil {
			httpError(w, "error getting shard: %v", err)
			return
		}
		w.Write(result)
	})

	cellShardTabletsCache := newCellShardTabletsCache(ts)
	http.HandleFunc("/json/CellShardTablets", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		cell := r.FormValue("cell")
		if cell == "" {
			http.Error(w, "no cell provided", http.StatusBadRequest)
			return
		}
		keyspace := r.FormValue("keyspace")
		if keyspace == "" {
			http.Error(w, "no keyspace provided", http.StatusBadRequest)
			return
		}
		shard := r.FormValue("shard")
		if shard == "" {
			http.Error(w, "no shard provided", http.StatusBadRequest)
			return
		}
		ctx := context.Background()
		result, err := cellShardTabletsCache.Get(ctx, cell+"/"+keyspace+"/"+shard)
		if err != nil {
			httpError(w, "error getting shard: %v", err)
			return
		}
		w.Write(result)
	})

	// flush all data and will force a full client reload
	http.HandleFunc("/json/flush", func(w http.ResponseWriter, r *http.Request) {
		knownCellsCache.Flush()
		keyspacesCache.Flush()
		keyspaceCache.Flush()
		shardNamesCache.Flush()
		shardCache.Flush()
		cellShardTabletsCache.Flush()
	})

	// handle tablet cache
	tabletHealthCache := newTabletHealthCache(ts, tmclient.NewTabletManagerClient())
	http.HandleFunc("/json/TabletHealth", func(w http.ResponseWriter, r *http.Request) {
		cell := r.FormValue("cell")
		if cell == "" {
			http.Error(w, "no cell provided", http.StatusBadRequest)
			return
		}
		uid := r.FormValue("uid")
		if uid == "" {
			http.Error(w, "no uid provided", http.StatusBadRequest)
			return
		}
		tabletAlias := topo.TabletAlias{
			Cell: cell,
		}
		var err error
		tabletAlias.Uid, err = topo.ParseUID(uid)
		if err != nil {
			http.Error(w, "cannot parse uid", http.StatusBadRequest)
			return
		}
		result, err := tabletHealthCache.get(tabletAlias)
		if err != nil {
			httpError(w, "error getting tablet health: %v", err)
			return
		}
		w.Write(result)
	})
	http.HandleFunc("/json/schema-manager", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			httpError(w, "cannot parse form: %s", err)
			return
		}
		sqlStr := r.FormValue("data")
		keyspace := r.FormValue("keyspace")
		executor := schemamanager.NewTabletExecutor(
			tmclient.NewTabletManagerClient(),
			ts)

		schemamanager.Run(
			context.Background(),
			schemamanager.NewUIController(sqlStr, keyspace, w),
			executor,
		)
	})
	if *schemaChangeDir != "" {
		interval := 60
		if *schemaChangeCheckInterval > 0 {
			interval = *schemaChangeCheckInterval
		}
		timer := timer.NewTimer(time.Duration(interval) * time.Second)
		controllerFactory, err :=
			schemamanager.GetControllerFactory(*schemaChangeController)
		if err != nil {
			log.Fatalf("unable to get a controller factory, error: %v", err)
		}

		timer.Start(func() {
			controller, err := controllerFactory(map[string]string{
				schemamanager.SchemaChangeDirName: *schemaChangeDir,
				schemamanager.SchemaChangeUser:    *schemaChangeUser,
			})
			if err != nil {
				log.Errorf("failed to get controller, error: %v", err)
				return
			}

			err = schemamanager.Run(
				context.Background(),
				controller,
				schemamanager.NewTabletExecutor(
					tmclient.NewTabletManagerClient(), ts),
			)
			log.Errorf("Schema change failed, error: %v", err)
		})
		servenv.OnClose(func() { timer.Stop() })
	}
	servenv.RunDefault()
}
