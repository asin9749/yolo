package yolosvc

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"reflect"
	"sort"
	"time"

	"berty.tech/yolo/v2/pkg/bintray"
	"berty.tech/yolo/v2/pkg/plistgen"
	"berty.tech/yolo/v2/pkg/yolopb"
	"github.com/buildkite/go-buildkite/buildkite"
	"github.com/cayleygraph/cayley"
	cayleypath "github.com/cayleygraph/cayley/query/path"
	"github.com/cayleygraph/cayley/schema"
	"github.com/cayleygraph/quad"
	"github.com/go-chi/chi"
	circleci "github.com/jszwedko/go-circleci"
	"github.com/stretchr/signature"
	"go.uber.org/zap"
)

type Service interface {
	yolopb.YoloServiceServer
	PlistGenerator(w http.ResponseWriter, r *http.Request)
	ArtifactDownloader(w http.ResponseWriter, r *http.Request)
}

type service struct {
	startTime time.Time
	db        *cayley.Handle
	logger    *zap.Logger
	schema    *schema.Config
	bkc       *buildkite.Client
	btc       *bintray.Client
	ccc       *circleci.Client
	authSalt  string
}

type ServiceOpts struct {
	BuildkiteClient *buildkite.Client
	CircleciClient  *circleci.Client
	BintrayClient   *bintray.Client
	Logger          *zap.Logger
	AuthSalt        string
}

func NewService(db *cayley.Handle, schema *schema.Config, opts ServiceOpts) Service {
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	return &service{
		startTime: time.Now(),
		db:        db,
		logger:    opts.Logger,
		schema:    schema,
		bkc:       opts.BuildkiteClient,
		btc:       opts.BintrayClient,
		ccc:       opts.CircleciClient,
		authSalt:  opts.AuthSalt,
	}
}

func (svc service) Ping(ctx context.Context, req *yolopb.Ping_Request) (*yolopb.Ping_Response, error) {
	return &yolopb.Ping_Response{}, nil
}

func (svc service) Status(ctx context.Context, req *yolopb.Status_Request) (*yolopb.Status_Response, error) {
	resp := yolopb.Status_Response{
		Uptime: int32(time.Since(svc.startTime).Seconds()),
	}

	// db
	stats, err := svc.db.Stats(ctx, false)
	if err == nil {
		resp.DbNodes = stats.Nodes.Value
		resp.DbQuads = stats.Quads.Value
	} else {
		resp.DbErr = err.Error()
	}

	return &resp, nil
}

func (svc service) BuildList(ctx context.Context, req *yolopb.BuildList_Request) (*yolopb.BuildList_Response, error) {
	resp := yolopb.BuildList_Response{}

	p := cayleypath.StartPath(svc.db).
		Has(quad.IRI("rdf:type"), quad.IRI("yolo:Build"))
	if req.ArtifactKind > 0 {
		// this will filter builds with at least one artifact of the good kind
		// but I don't know how to filter them during loading, so I will cleanup
		// the result later, feel free to help me make things in a better way
		p = p.HasPath(
			cayleypath.StartMorphism().
				In(quad.IRI("schema:hasBuild")).
				Has(quad.IRI("schema:kind"), quad.Int(req.ArtifactKind)),
		)
	}
	p = p.Limit(300)

	builds := []yolopb.Build{}
	if err := svc.schema.LoadIteratorToDepth(ctx, svc.db, reflect.ValueOf(&builds), 1, p.BuildIterator(ctx)); err != nil {
		return nil, fmt.Errorf("load builds: %w", err)
	}
	// clean up the result and add signed URLs
	for idx, build := range builds {
		build.Cleanup()
		// cleanup artifact with invalid requested type (see comment above)
		if req.ArtifactKind > 0 {
			n := 0
			for _, artifact := range build.HasArtifacts {
				if artifact.Kind == req.ArtifactKind {
					build.HasArtifacts[n] = artifact
					n++
				}
			}
			builds[idx].HasArtifacts = build.HasArtifacts[:n]
		}
		if err := build.AddSignedURLs(svc.authSalt); err != nil {
			return nil, fmt.Errorf("sign URLs")
		}
	}

	resp.Builds = make([]*yolopb.Build, len(builds))
	for i := range builds {
		resp.Builds[i] = &builds[i]
	}

	sort.Slice(resp.Builds[:], func(i, j int) bool {
		if resp.Builds[j] == nil || resp.Builds[j].CreatedAt == nil {
			return true
		}
		if resp.Builds[i] == nil || resp.Builds[i].CreatedAt == nil {
			return false
		}
		return resp.Builds[i].CreatedAt.After(*resp.Builds[j].CreatedAt)
	})

	return &resp, nil
}

func (svc service) PlistGenerator(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "artifactID")

	p := cayleypath.
		StartPath(svc.db, quad.IRI(id)).
		Has(quad.IRI("rdf:type"), quad.IRI("yolo:Artifact"))
	var artifact yolopb.Artifact
	if err := svc.schema.LoadPathTo(r.Context(), svc.db, &artifact, p); err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusInternalServerError)
		return
	}

	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)
	var (
		bundleID = "tech.berty.ios" // FIXME: change me
		title    = "YOLO"           // FIXME: use random emojis :)
		version  = "v0.0.1"         // FIXME: change me
		url      = "/api/artifact-dl/" + id
	)

	var err error
	url, err = signature.GetSignedURL("GET", url, "", svc.authSalt)
	if err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusInternalServerError)
		return
	}

	url = baseURL + url // prepend baseURL _after_ computing the signature

	b, err := plistgen.Release(bundleID, version, title, url)
	if err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Add("Content-Type", "application/x-plist")
	_, _ = w.Write(b)
}

func (svc service) ArtifactDownloader(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "artifactID")

	p := cayleypath.
		StartPath(svc.db, quad.IRI(id)).
		Has(quad.IRI("rdf:type"), quad.IRI("yolo:Artifact"))
	var artifact yolopb.Artifact
	if err := svc.schema.LoadPathTo(r.Context(), svc.db, &artifact, p); err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusInternalServerError)
		return
	}
	artifact.Cleanup()

	base := path.Base(artifact.LocalPath)
	w.Header().Add("Content-Disposition", fmt.Sprintf("inline; filename=%s", base))
	if artifact.FileSize > 0 {
		w.Header().Add("Content-Length", fmt.Sprintf("%d", artifact.FileSize))
	}
	if artifact.MimeType != "" {
		w.Header().Add("Content-Type", artifact.MimeType)
	}

	var err error
	switch artifact.Driver {
	case yolopb.Driver_Buildkite:
		if svc.bkc == nil {
			err = fmt.Errorf("buildkite token required")
		} else {
			_, err = svc.bkc.Artifacts.DownloadArtifactByURL(artifact.DownloadURL, w)
		}
	case yolopb.Driver_Bintray:
		err = bintray.DownloadContent(artifact.DownloadURL, w)
	// case Driver_CircleCI:
	default:
		err = fmt.Errorf("download not supported for this driver")
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusInternalServerError)
	}
}
