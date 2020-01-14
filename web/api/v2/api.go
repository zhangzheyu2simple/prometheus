// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api_v2

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/pkg/errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/timestamp"
	pb "github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql"

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
)

// API encapsulates all API services.
type API struct {
	engine  *promql.Engine
	storage storage.Storage

	enableAdmin bool
	db          func() *tsdb.DB
}

// New returns a new API object.
func New(
	db func() *tsdb.DB,
	enableAdmin bool,
	engine *promql.Engine,
	storage storage.Storage,
) *API {
	return &API{
		db:          db,
		enableAdmin: enableAdmin,
		engine:      engine,
		storage:     storage,
	}
}

// RegisterGRPC registers all API services with the given server.
func (api *API) RegisterGRPC(srv *grpc.Server) {
	fmt.Println("regis")
	pb.RegisterApiServer(srv, NewQueryServer(api.engine, api.storage))
	if api.enableAdmin {
		pb.RegisterAdminServer(srv, NewAdmin(api.db))
	} else {
		pb.RegisterAdminServer(srv, &AdminDisabled{})
	}
}

// HTTPHandler returns an HTTP handler for a REST API gateway to the given grpc address.
func (api *API) HTTPHandler(ctx context.Context, grpcAddr string) (http.Handler, error) {
	enc := new(runtime.JSONPb)
	mux := runtime.NewServeMux(runtime.WithMarshalerOption(enc.ContentType(), enc))

	opts := []grpc.DialOption{
		grpc.WithInsecure(),
		// Replace the default dialer that connects through proxy when HTTP_PROXY is set.
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}),
	}

	err := pb.RegisterAdminHandlerFromEndpoint(ctx, mux, grpcAddr, opts)
	if err != nil {
		return nil, err
	}
	return mux, nil
}

// extractTimeRange returns minimum and maximum timestamp in milliseconds as
// provided by the time range. It defaults either boundary to the minimum and maximum
// possible value.
func extractTimeRange(min, max *time.Time) (mint, maxt time.Time, err error) {
	if min == nil {
		mint = minTime
	} else {
		mint = *min
	}
	if max == nil {
		maxt = maxTime
	} else {
		maxt = *max
	}
	if mint.After(maxt) {
		return mint, maxt, errors.Errorf("min time must be before or equal to max time")
	}
	return mint, maxt, nil
}

var (
	minTime = time.Unix(math.MinInt64/1000+62135596801, 0)
	maxTime = time.Unix(math.MaxInt64/1000-62135596801, 999999999)
)

var (
	errAdminDisabled = status.Error(codes.Unavailable, "Admin APIs are disabled")
	errTSDBNotReady  = status.Error(codes.Unavailable, "TSDB not ready")
)

// AdminDisabled implements the administration interface that informs
// that the API endpoints are disabled.
type AdminDisabled struct {
}

// TSDBSnapshot implements pb.AdminServer.
func (s *AdminDisabled) TSDBSnapshot(_ context.Context, _ *pb.TSDBSnapshotRequest) (*pb.TSDBSnapshotResponse, error) {
	return nil, errAdminDisabled
}

// TSDBCleanTombstones implements pb.AdminServer.
func (s *AdminDisabled) TSDBCleanTombstones(_ context.Context, _ *pb.TSDBCleanTombstonesRequest) (*pb.TSDBCleanTombstonesResponse, error) {
	return nil, errAdminDisabled
}

// DeleteSeries implements pb.AdminServer.
func (s *AdminDisabled) DeleteSeries(_ context.Context, r *pb.SeriesDeleteRequest) (*pb.SeriesDeleteResponse, error) {
	return nil, errAdminDisabled
}

// Admin provides an administration interface to Prometheus.
type Admin struct {
	db func() *tsdb.DB
}

// NewAdmin returns a Admin server.
func NewAdmin(db func() *tsdb.DB) *Admin {
	return &Admin{
		db: db,
	}
}

// TSDBSnapshot implements pb.AdminServer.
func (s *Admin) TSDBSnapshot(_ context.Context, req *pb.TSDBSnapshotRequest) (*pb.TSDBSnapshotResponse, error) {
	db := s.db()
	if db == nil {
		return nil, errTSDBNotReady
	}
	var (
		snapdir = filepath.Join(db.Dir(), "snapshots")
		name    = fmt.Sprintf("%s-%x",
			time.Now().UTC().Format("20060102T150405Z0700"),
			rand.Int())
		dir = filepath.Join(snapdir, name)
	)
	if err := os.MkdirAll(dir, 0777); err != nil {
		return nil, status.Errorf(codes.Internal, "created snapshot directory: %s", err)
	}
	if err := db.Snapshot(dir, !req.SkipHead); err != nil {
		return nil, status.Errorf(codes.Internal, "create snapshot: %s", err)
	}
	return &pb.TSDBSnapshotResponse{Name: name}, nil
}

// TSDBCleanTombstones implements pb.AdminServer.
func (s *Admin) TSDBCleanTombstones(_ context.Context, _ *pb.TSDBCleanTombstonesRequest) (*pb.TSDBCleanTombstonesResponse, error) {
	db := s.db()
	if db == nil {
		return nil, errTSDBNotReady
	}

	if err := db.CleanTombstones(); err != nil {
		return nil, status.Errorf(codes.Internal, "clean tombstones: %s", err)
	}

	return &pb.TSDBCleanTombstonesResponse{}, nil
}

// DeleteSeries implements pb.AdminServer.
func (s *Admin) DeleteSeries(_ context.Context, r *pb.SeriesDeleteRequest) (*pb.SeriesDeleteResponse, error) {
	mint, maxt, err := extractTimeRange(r.MinTime, r.MaxTime)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	var matchers []*labels.Matcher

	for _, m := range r.Matchers {
		var lm *labels.Matcher
		var err error

		switch m.Type {
		case pb.LabelMatcher_EQ:
			lm, err = labels.NewMatcher(labels.MatchEqual, m.Name, m.Value)
		case pb.LabelMatcher_NEQ:
			lm, err = labels.NewMatcher(labels.MatchNotEqual, m.Name, m.Value)
		case pb.LabelMatcher_RE:
			lm, err = labels.NewMatcher(labels.MatchRegexp, m.Name, m.Value)
		case pb.LabelMatcher_NRE:
			lm, err = labels.NewMatcher(labels.MatchNotRegexp, m.Name, m.Value)
		default:
			return nil, status.Error(codes.InvalidArgument, "unknown matcher type")
		}

		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "bad matcher: %s", err)
		}

		matchers = append(matchers, lm)
	}
	db := s.db()
	if db == nil {
		return nil, errTSDBNotReady
	}
	if err := db.Delete(timestamp.FromTime(mint), timestamp.FromTime(maxt), matchers...); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.SeriesDeleteResponse{}, nil
}

// NewAdmin returns a Admin server.
func NewQueryServer(qe *promql.Engine, q storage.Queryable) *QueryServer {
	return &QueryServer{
		QueryEngine: qe,
		Queryable:   q,
	}
}

type QueryServer struct {
	QueryEngine *promql.Engine
	Queryable   storage.Queryable
}

func (q *QueryServer) InstantQuery(ctx context.Context, in *pb.InstantQueryRequest) (*pb.InstantQueryResponse, error) {
	var ts time.Time
	if in.TimeStamp != nil {
		ts = *in.TimeStamp
	} else {
		ts = time.Now()
	}

	qry, err := q.QueryEngine.NewInstantQuery(q.Queryable, in.Query, ts)
	if err != nil {
		err = errors.Wrapf(err, "invalid parameter 'query'")
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	res := qry.Exec(ctx)
	if res.Err != nil {
		return nil, status.Error(codes.Internal, res.Err.Error())
	}
	resp := pb.InstantQueryResponse{
		VauleType: string(res.Value.Type()),
	}
	switch t := res.Value.Type(); t {
	case promql.ValueTypeMatrix:
		matrix := res.Value.(promql.Matrix)
		resp.Matrix = generateProtoMatrix(matrix)
	case promql.ValueTypeVector:
		vector := res.Value.(promql.Vector)
		resp.Vector = generateProtoVector(vector)
	}
	return &resp, nil
}
func (q *QueryServer) RangeQuery(ctx context.Context, in *pb.RangeQueryRequest) (*pb.RangeQueryResponse, error) {
	start, end, err := extractTimeRange(in.Start, in.End)

	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	ts := *in.Step
	fmt.Println(start, end, ts)
	qry, err := q.QueryEngine.NewRangeQuery(q.Queryable, in.Query, start, end, ts)

	if err != nil {
		err = errors.Wrapf(err, "invalid parameter 'query'")
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	res := qry.Exec(ctx)
	if res.Err != nil {
		return nil, status.Error(codes.Internal, res.Err.Error())
	}
	//Optional stats field in response if parameter "stats" is not empty.
	resp := pb.RangeQueryResponse{
		VauleType: string(res.Value.Type()),
	}
	switch t := res.Value.Type(); t {
	case promql.ValueTypeMatrix:
		matrix := res.Value.(promql.Matrix)
		fmt.Println(matrix)
		resp.Matrix = generateProtoMatrix(matrix)
	}

	return &resp, nil

}

func generateProtoMatrix(matrix promql.Matrix) *pb.Matrix {
	seriesArray := make([]*pb.TimeSeries, len(matrix))
	for index, series := range matrix {
		labels := make([]pb.Label, len(series.Metric))
		for j, label := range series.Metric {
			labels[j] = pb.Label{Name: label.Name, Value: label.Value}
		}
		samples := make([]pb.Sample, len(series.Points))
		for k, point := range series.Points {
			samples[k] = pb.Sample{Value: point.V, Timestamp: point.T}
		}

		pbSeries := pb.TimeSeries{Labels: labels, Samples: samples}
		seriesArray[index] = &pbSeries
	}
	return &pb.Matrix{Series: seriesArray}
}
func generateProtoVector(vector promql.Vector) *pb.Vector {
	labeledSamples := make([]*pb.LabeledSample, len(vector))
	for index, sample := range vector {
		labels := make([]pb.Label, len(sample.Metric))
		for j, label := range sample.Metric {
			labels[j] = pb.Label{Name: label.Name, Value: label.Value}
		}
		labeledSample := pb.LabeledSample{Labels: labels, Point: pb.Sample{Value: sample.V, Timestamp: sample.T}}
		labeledSamples[index] = &labeledSample
	}
	return &pb.Vector{Samples: labeledSamples}
}
