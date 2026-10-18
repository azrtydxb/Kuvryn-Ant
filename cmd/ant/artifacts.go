package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/ansiblerepo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/apkrepo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/aptrepo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/cargo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/cocoapods"
	"github.com/azrtydxb/Kuvryn-Ant/internal/composer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/conan"
	"github.com/azrtydxb/Kuvryn-Ant/internal/condarepo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/cran"
	"github.com/azrtydxb/Kuvryn-Ant/internal/gitlfs"
	"github.com/azrtydxb/Kuvryn-Ant/internal/goproxy"
	"github.com/azrtydxb/Kuvryn-Ant/internal/helmrepo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/huggingface"
	"github.com/azrtydxb/Kuvryn-Ant/internal/maven"
	"github.com/azrtydxb/Kuvryn-Ant/internal/npmregistry"
	"github.com/azrtydxb/Kuvryn-Ant/internal/nuget"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oci"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/p2repo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/pubrepo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/pypi"
	"github.com/azrtydxb/Kuvryn-Ant/internal/rawrepo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteruntime"
	"github.com/azrtydxb/Kuvryn-Ant/internal/rpmrepo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/rubygems"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/azrtydxb/Kuvryn-Ant/internal/swiftrepo"
	"github.com/azrtydxb/Kuvryn-Ant/internal/terraform"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Artifact serving is activated after local bootstrap (or with an explicit
// instance ID). The writer lease covers the entire serving lifetime.
type artifactRuntime struct {
	conan       *conan.Repository
	apk         *apkrepo.Repository
	apt         *aptrepo.Repository
	rpm         *rpmrepo.Repository
	terraform   *terraform.Repository
	swift       *swiftrepo.Repository
	cocoapods   *cocoapods.Repository
	huggingface *huggingface.Repository
	p2          *p2repo.Repository
	conda       *condarepo.Repository
	ansible     *ansiblerepo.Repository
	cran        *cran.Repository
	rubygems    *rubygems.Repository
	pubrepo     *pubrepo.Repository
	composer    *composer.Repository
	gitlfs      *gitlfs.Repository
	instanceID  string
	nuget       *nuget.Repository
	maven       *maven.Repository
	pypi        *pypi.Repository
	npm         *npmregistry.Registry
	manager     *ownership.Manager
	stores      *storage.Registry
	migrator    *storage.Migrator
	registry    *oci.Registry
	goProxy     *goproxy.Proxy
	service     *artifact.Service
	cargo       *cargo.Registry
	helm        *helmrepo.Repository
	raw         *rawrepo.Repository
	remote      *remoteruntime.Manager
}

func openArtifacts(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) (*artifactRuntime, error) {
	instance := cfg.InstanceID
	var owner string
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if instance == "" {
		err := pool.QueryRow(lookupCtx, "SELECT instance_id::text FROM bootstrap_state WHERE singleton=true").Scan(&instance)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, errors.New("cannot load serving instance")
		}
	}
	if err := pool.QueryRow(lookupCtx, "SELECT id::text,gen_random_uuid()::text FROM instances WHERE id=$1", instance).Scan(&instance, &owner); err != nil {
		return nil, errors.New("invalid serving instance")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Ownership outlives the signal context so in-flight HTTP requests can drain
	// while still fenced. artifactRuntime.Close releases after Serve returns.
	manager, err := ownership.Start(context.WithoutCancel(ctx), pool, instance, owner, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = manager.Close()
		return nil, err
	}
	stores, router, err := openStores(pool, cfg, instance)
	if err != nil {
		manager.Close()
		return nil, err
	}
	policies := artifact.Policies{DB: pool, InstanceID: instance}
	service := &artifact.Service{DB: pool, Lease: manager.Lease, Router: policies.Load, Storage: stores.Resolve}
	registry, err := oci.New(pool, service, filepath.Join(cfg.Storage.Directory, "uploads"))
	if err != nil {
		stores.Close()
		manager.Close()
		return nil, err
	}
	registry.SetMirror(oci.Mirror{DefaultFeed: cfg.OCIMirror.DefaultFeed, Namespaces: cfg.OCIMirror.Namespaces})
	if err := ctx.Err(); err != nil {
		registry.Close()
		stores.Close()
		_ = manager.Close()
		return nil, err
	}
	goProxy, err := goproxy.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	cargoRegistry, err := cargo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	rawRepository, err := rawrepo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	npmRegistry, err := npmregistry.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	nugetRepository, err := nuget.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	mavenRepository, err := maven.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	pypiRepository, err := pypi.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	gitlfsRepository, err := gitlfs.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	composerRepository, err := composer.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	rubygemsRepository, err := rubygems.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	pubrepoRepository, err := pubrepo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	conanRepository, err := conan.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	condaRepository, err := condarepo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	ansibleRepository, err := ansiblerepo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	cranRepository, err := cran.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	apkRepository, err := apkrepo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	aptRepository, err := aptrepo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	rpmRepository, err := rpmrepo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	terraformRepository, err := terraform.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	swiftRepository, err := swiftrepo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	cocoapodsRepository, err := cocoapods.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	huggingfaceRepository, err := huggingface.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	p2Repository, err := p2repo.New(pool, service)
	if err != nil {
		registry.Close()
		stores.Close()
		manager.Close()
		return nil, err
	}
	if cfg.SecretKeys != nil {
		aptRepository.SetKeyring(cfg.SecretKeys)
		apkRepository.SetKeyring(cfg.SecretKeys)
		if err := rpmRepository.SetSecrets(secrets.Store{DB: pool, Keys: cfg.SecretKeys}); err != nil {
			registry.Close()
			stores.Close()
			manager.Close()
			return nil, err
		}
	}
	// 24 MiB metadata documents: web-sys crates.io index (4.4 MiB) and the typescript packument (8.7 MiB abbreviated, 16 MiB full) exceed the old limits.
	// Pull-through reads: the runtime resolves per-source configuration, the
	// source-bound credential and the operator CA map (IDs only; paths come from
	// ANT_UPSTREAM_CA_MAP_FILE, never from requests) on demand. Without the
	// secret keyring no remote source can authenticate, so pull-through stays off
	// and feeds serve local content only.
	var remote *remoteruntime.Manager
	if cfg.SecretKeys != nil {
		remote, err = remoteruntime.New(pool, instance, cfg.SecretKeys, remoteruntime.Options{CAFiles: cfg.UpstreamCAs, MaxMetadataBytes: 24 << 20, MaxCacheBytes: 256 << 20})
		if err != nil {
			registry.Close()
			stores.Close()
			manager.Close()
			return nil, err
		}
		service.Remote = remote
	}
	return &artifactRuntime{remote: remote, apk: apkRepository, apt: aptRepository, rpm: rpmRepository, terraform: terraformRepository, swift: swiftRepository, cocoapods: cocoapodsRepository, huggingface: huggingfaceRepository, p2: p2Repository, conan: conanRepository, conda: condaRepository, ansible: ansibleRepository, cran: cranRepository, rubygems: rubygemsRepository, gitlfs: gitlfsRepository, composer: composerRepository, pubrepo: pubrepoRepository, instanceID: instance, nuget: nugetRepository, maven: mavenRepository, pypi: pypiRepository, manager: manager, stores: stores, migrator: newMigrator(pool, cfg, instance, stores, router), registry: registry, goProxy: goProxy, service: service, cargo: cargoRegistry, helm: helmrepo.New(pool, service), raw: rawRepository, npm: npmRegistry}, nil
}
func (r *artifactRuntime) Register(mux *http.ServeMux) {
	if r != nil {
		mux.Handle("/v2/", r.registry)
		mux.Handle("/feeds/{slug}/rubygems/", r.rubygems)
		mux.Handle("/feeds/{slug}/git-lfs/", r.gitlfs)
		mux.Handle("/feeds/{slug}/composer/", r.composer)
		mux.Handle("/feeds/{slug}/pub/", r.pubrepo)
		mux.Handle("/feeds/{slug}/conan/", r.conan)
		mux.Handle("/feeds/{slug}/apk/", r.apk)
		mux.Handle("/feeds/{slug}/apt/", r.apt)
		mux.Handle("/feeds/{slug}/rpm/", r.rpm)
		mux.Handle("/feeds/{slug}/terraform/", r.terraform)
		mux.Handle("/feeds/{slug}/swift/", r.swift)
		mux.Handle("/feeds/{slug}/cocoapods/", r.cocoapods)
		mux.Handle("/feeds/{slug}/huggingface/", r.huggingface)
		mux.Handle("/feeds/{slug}/p2/", r.p2)
		mux.Handle("/feeds/{slug}/conda/", r.conda)
		mux.Handle("/feeds/{slug}/ansible/", r.ansible)
		mux.Handle("/feeds/{slug}/cran/", r.cran)
		mux.Handle("/feeds/{slug}/nuget/", r.nuget)
		mux.Handle("/feeds/{slug}/maven/", r.maven)
		mux.Handle("/feeds/{slug}/pypi/", r.pypi)
		mux.Handle("/feeds/{slug}/npm/", r.npm)
		mux.Handle("/feeds/{slug}/go/", r.goProxy)
		mux.Handle("/feeds/{slug}/helm/", r.helm)
		mux.Handle("/feeds/{slug}/cargo/", r.cargo)
		mux.Handle("/feeds/{slug}/raw/", r.raw)
	}
}
func (r *artifactRuntime) Close() {
	if r != nil {
		if r.remote != nil {
			_ = r.remote.Close() // cancel in-flight upstream streams before storage closes
		}
		r.pubrepo.Close()
		r.registry.Close()
		r.stores.Close()
		_ = r.manager.Close()
	}
}
