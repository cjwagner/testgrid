/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package updater

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/sirupsen/logrus"
	"vbom.ml/util/sortorder"

	"github.com/GoogleCloudPlatform/testgrid/config"
	"github.com/GoogleCloudPlatform/testgrid/internal/result"
	configpb "github.com/GoogleCloudPlatform/testgrid/pb/config"
	"github.com/GoogleCloudPlatform/testgrid/pb/state"
	"github.com/GoogleCloudPlatform/testgrid/util/gcs"
)

func Update(client *storage.Client, parent context.Context, configPath gcs.Path, gridPrefix string, groupConcurrency int, buildConcurrency int, confirm bool, groupTimeout time.Duration, buildTimeout time.Duration, group string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	log := logrus.WithField("config", configPath)
	cfg, err := config.ReadGCS(ctx, client.Bucket(configPath.Bucket()).Object(configPath.Object()))
	if err != nil {
		return err
	}
	log.WithField("groups", len(cfg.TestGroups)).Info("Updating test groups")

	groups := make(chan configpb.TestGroup)
	var wg sync.WaitGroup

	gc := realGCSClient{client: client}
	for i := 0; i < groupConcurrency; i++ {
		wg.Add(1)
		go func() {
			for tg := range groups {
				location := path.Join(gridPrefix, tg.Name)
				tgp, err := testGroupPath(configPath, location)
				if err == nil {
					err = updateGroup(ctx, gc, tg, *tgp, buildConcurrency, confirm, groupTimeout, buildTimeout)
				}
				if err != nil {
					log.WithField("group", tg.Name).WithError(err).Error("Error updating group")
				}
			}
			wg.Done()
		}()
	}

	if group != "" { // Just a specific group
		tg := config.FindTestGroup(group, cfg)
		if tg == nil {
			return errors.New("group not found")
		}
		groups <- *tg
	} else { // All groups
		idxChan := make(chan int)
		defer close(idxChan)
		go logUpdate(idxChan, len(cfg.TestGroups), "Update in progress")
		for i, tg := range cfg.TestGroups {
			select {
			case idxChan <- i:
			default:
			}
			groups <- *tg
		}
	}
	close(groups)
	wg.Wait()
	return nil
}

// testGroupPath() returns the path to a test_group proto given this proto
func testGroupPath(g gcs.Path, name string) (*gcs.Path, error) {
	u, err := url.Parse(name)
	if err != nil {
		return nil, fmt.Errorf("invalid url %s: %v", name, err)
	}
	np, err := g.ResolveReference(u)
	if err == nil && np.Bucket() != g.Bucket() {
		return nil, fmt.Errorf("testGroup %s should not change bucket", name)
	}
	return np, nil
}

// logUpdate posts Update progress every minute, including an ETA for completion.
func logUpdate(ch <-chan int, total int, msg string) {
	start := time.Now()
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	var current int
	var ok bool
	for {
		select {
		case current, ok = <-ch:
			if !ok { // channel is closed
				return
			}
		case now := <-timer.C:
			elapsed := now.Sub(start)
			rate := elapsed / time.Duration(current)
			eta := time.Duration(total-current) * rate

			logrus.WithFields(logrus.Fields{
				"current": current,
				"total":   total,
				"percent": (100 * current) / total,
				"remain":  eta.Round(time.Minute),
				"eta":     now.Add(eta).Round(time.Minute),
			}).Info(msg)
			timer.Reset(time.Minute)
		}
	}
}

type gcsUploadClient interface {
	gcsClient
	Upload(context.Context, gcs.Path, []byte, bool, string) error
}

type realGCSClient struct {
	client *storage.Client
}

func (rgc realGCSClient) Open(ctx context.Context, path gcs.Path) (io.ReadCloser, error) {
	r, err := rgc.client.Bucket(path.Bucket()).Object(path.Object()).NewReader(ctx)
	return r, err
}

func (rgc realGCSClient) Objects(ctx context.Context, path gcs.Path, delimiter string) gcs.Iterator {
	p := path.Object()
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return rgc.client.Bucket(path.Bucket()).Objects(ctx, &storage.Query{
		Delimiter: delimiter,
		Prefix:    p,
	})
}

func (rgc realGCSClient) Upload(ctx context.Context, path gcs.Path, buf []byte, worldReadable bool, cacheControl string) error {
	return gcs.Upload(ctx, rgc.client, path, buf, worldReadable, cacheControl)
}

func updateGroup(parent context.Context, client gcsUploadClient, tg configpb.TestGroup, gridPath gcs.Path, concurrency int, write bool, groupTimeout, buildTimeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, groupTimeout)
	defer cancel()
	log := logrus.WithField("group", tg.Name)

	var tgPath gcs.Path
	if err := tgPath.Set("gs://" + tg.GcsPrefix); err != nil {
		return fmt.Errorf("set group path: %w", err)
	}

	builds, err := gcs.ListBuilds(ctx, client, tgPath)
	if err != nil {
		return fmt.Errorf("list builds: %w", err)
	}
	log.WithField("total", len(builds)).Debug("Listed builds")
	var dur time.Duration
	if tg.DaysOfResults > 0 {
		dur = days(float64(tg.DaysOfResults))
	} else {
		dur = days(7)
	}
	const maxCols = 50

	stop := time.Now().Add(-dur)
	cols, err := readColumns(ctx, client, tg, builds, stop, maxCols, buildTimeout, concurrency)
	if err != nil {
		return fmt.Errorf("read columns: %w", err)
	}

	grid := constructGrid(tg, cols)
	buf, err := marshalGrid(grid)
	if err != nil {
		return fmt.Errorf("marshal grid: %w", err)
	}
	log = log.WithField("url", gridPath).WithField("bytes", len(buf))
	if !write {
		log.Debug("Skipping write")
	} else {
		log.Debug("Writing")
		// TODO(fejta): configurable cache value
		if err := client.Upload(ctx, gridPath, buf, gcs.DefaultAcl, "no-cache"); err != nil {
			return fmt.Errorf("upload: %w", err)
		}
	}
	log.WithFields(logrus.Fields{
		"cols": len(grid.Columns),
		"rows": len(grid.Rows),
	}).Info("Wrote grid")
	return nil
}

// days converts days float into a time.Duration, assuming a 24 hour day.
//
// A day is not always 24 hours due to things like leap-seconds.
// We do not need this level of precision though, so ignore the complexity.
func days(d float64) time.Duration {
	return time.Duration(24*d) * time.Hour // Close enough
}

// constructGrid will append all the inflatedColumns into the returned Grid.
//
// The returned Grid has correctly compressed row values.
func constructGrid(group configpb.TestGroup, cols []inflatedColumn) state.Grid {
	// Add the columns into a grid message
	var grid state.Grid
	rows := map[string]*state.Row{} // For fast target => row lookup
	failsOpen := int(group.NumFailuresToAlert)
	passesClose := int(group.NumPassesToDisableAlert)
	if failsOpen > 0 && passesClose == 0 {
		passesClose = 1
	}

	for _, col := range cols {
		appendColumn(&grid, rows, col)
		alertRows(grid.Columns, grid.Rows, failsOpen, passesClose)
	}
	sort.SliceStable(grid.Rows, func(i, j int) bool {
		return sortorder.NaturalLess(grid.Rows[i].Name, grid.Rows[j].Name)
	})

	for _, row := range grid.Rows {
		sort.SliceStable(row.Metric, func(i, j int) bool {
			return sortorder.NaturalLess(row.Metric[i], row.Metric[j])
		})
		sort.SliceStable(row.Metrics, func(i, j int) bool {
			return sortorder.NaturalLess(row.Metrics[i].Name, row.Metrics[j].Name)
		})
	}
	return grid
}

// marhshalGrid serializes a state proto into zlib-compressed bytes.
func marshalGrid(grid state.Grid) ([]byte, error) {
	buf, err := proto.Marshal(&grid)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err = zw.Write(buf); err != nil {
		return nil, fmt.Errorf("compress: %w", err)
	}
	if err = zw.Close(); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	return zbuf.Bytes(), nil
}

// appendMetric adds the value at index to metric.
//
// Handles the details of sparse-encoding the results.
// Indices must be monotonically increasing for the same metric.
func appendMetric(metric *state.Metric, idx int32, value float64) {
	if l := int32(len(metric.Indices)); l == 0 || metric.Indices[l-2]+metric.Indices[l-1] != idx {
		// If we append V to idx 9 and metric.Indices = [3, 4] then the last filled index is 3+4-1=7
		// So that means we have holes in idx 7 and 8, so start a new group.
		metric.Indices = append(metric.Indices, idx, 1)
	} else {
		metric.Indices[l-1]++ // Expand the length of the current filled list
	}
	metric.Values = append(metric.Values, value)
}

var emptyCell = cell{result: state.Row_NO_RESULT}

// appendCell adds the rowResult column to the row.
//
// Handles the details like missing fields and run-length-encoding the result.
func appendCell(row *state.Row, cell cell, count int) {
	latest := int32(cell.result)
	n := len(row.Results)
	switch {
	case n == 0, row.Results[n-2] != latest:
		row.Results = append(row.Results, latest, int32(count))
	default:
		row.Results[n-1] += int32(count)
	}

	for i := 0; i < count; i++ {
		row.CellIds = append(row.CellIds, cell.cellID)
		if cell.result == state.Row_NO_RESULT {
			continue
		}
		for metricName, measurement := range cell.metrics {
			var metric *state.Metric
			var ok bool
			for _, name := range row.Metric {
				if name == metricName {
					ok = true
					break
				}
			}
			if !ok {
				row.Metric = append(row.Metric, metricName)
			}
			for _, metric = range row.Metrics {
				if metric.Name == metricName {
					break
				}
				metric = nil
			}
			if metric == nil {
				metric = &state.Metric{Name: metricName}
				row.Metrics = append(row.Metrics, metric)
			}
			// len()-1 because we already appended the cell id
			appendMetric(metric, int32(len(row.CellIds)-1), measurement)
		}
		// Javascript client expects no result cells to skip icons/messages
		row.Messages = append(row.Messages, cell.message)
		row.Icons = append(row.Icons, cell.icon)
	}
}

type nameConfig struct {
	format string
	parts  []string
}

func makeNameConfig(tnc *configpb.TestNameConfig) nameConfig {
	if tnc == nil {
		return nameConfig{
			format: "%s",
			parts:  []string{"Tests name"},
		}
	}
	nc := nameConfig{
		format: tnc.NameFormat,
		parts:  make([]string, len(tnc.NameElements)),
	}
	for i, e := range tnc.NameElements {
		nc.parts[i] = e.TargetConfig
	}
	return nc
}

// appendColumn adds the build column to the grid.
//
// This handles details like:
// * rows appearing/disappearing in the middle of the run.
// * adding auto metadata like duration, commit as well as any user-added metadata
// * extracting build metadata into the appropriate column header
// * Ensuring row names are unique and formatted with metadata
func appendColumn(grid *state.Grid, rows map[string]*state.Row, inflated inflatedColumn) {
	grid.Columns = append(grid.Columns, inflated.column)

	missing := map[string]*state.Row{}
	for name, row := range rows {
		missing[name] = row
	}

	for name, cell := range inflated.cells {
		delete(missing, name)

		row, ok := rows[name]
		if !ok {
			row = &state.Row{
				Name: name,
				Id:   name,
			}
			rows[name] = row
			grid.Rows = append(grid.Rows, row)
			if n := len(grid.Columns); n > 1 {
				appendCell(row, emptyCell, n-1)
			}
		}
		appendCell(row, cell, 1)
	}

	for _, row := range missing {
		appendCell(row, emptyCell, 1)
	}
}

// alertRows configures the alert for every row that has one.
func alertRows(cols []*state.Column, rows []*state.Row, openFailures, closePasses int) {
	for _, r := range rows {
		r.AlertInfo = alertRow(cols, r, openFailures, closePasses)
	}
}

// alertRow returns an AlertInfo proto if there have been failuresToOpen consecutive failures more recently than passesToClose.
func alertRow(cols []*state.Column, row *state.Row, failuresToOpen, passesToClose int) *state.AlertInfo {
	if failuresToOpen == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var failures int
	var totalFailures int32
	var passes int
	var compressedIdx int
	ch := result.Iter(ctx, row.Results)
	var lastFail *state.Column
	var latestPass *state.Column
	var failIdx int
	// find the first number of consecutive passesToClose (no alert)
	// or else failuresToOpen (alert).
	for _, col := range cols {
		// TODO(fejta): ignore old running
		rawRes := <-ch
		res := result.Coalesce(rawRes, result.IgnoreRunning)
		if res == state.Row_NO_RESULT {
			if rawRes == state.Row_RUNNING {
				compressedIdx++
			}
			continue
		}
		if res == state.Row_PASS {
			passes++
			if failures >= failuresToOpen {
				latestPass = col // most recent pass before outage
				break
			}
			if passes >= passesToClose {
				return nil // there is no outage
			}
			failures = 0
		}
		if res == state.Row_FAIL {
			passes = 0
			failures++
			totalFailures++
			if failures == 1 { // note most recent failure for this outage
				failIdx = compressedIdx
			}
			lastFail = col
		}
		if res == state.Row_FLAKY {
			passes = 0
			if failures >= failuresToOpen {
				break // cannot definitively say which commit is at fault
			}
			failures = 0
		}
		compressedIdx++
	}
	if failures < failuresToOpen {
		return nil
	}
	msg := row.Messages[failIdx]
	id := row.CellIds[failIdx]
	return alertInfo(totalFailures, msg, id, lastFail, latestPass)
}

// alertInfo returns an alert proto with the configured fields
func alertInfo(failures int32, msg, cellId string, fail, pass *state.Column) *state.AlertInfo {
	return &state.AlertInfo{
		FailCount:      failures,
		FailBuildId:    buildID(fail),
		FailTime:       stamp(fail),
		FailTestId:     cellId,
		FailureMessage: msg,
		PassTime:       stamp(pass),
		PassBuildId:    buildID(pass),
	}
}

// buildID extracts the ID from the first extra row or else the Build field.
func buildID(col *state.Column) string {
	if col == nil {
		return ""
	}
	if len(col.Extra) > 0 {
		return col.Extra[0]
	}
	return col.Build
}

const billion = 1e9

// stamp converts seconds into a timestamp proto
func stamp(col *state.Column) *timestamp.Timestamp {
	if col == nil {
		return nil
	}
	seconds := col.Started
	floor := math.Floor(seconds)
	remain := seconds - floor
	return &timestamp.Timestamp{
		Seconds: int64(floor),
		Nanos:   int32(remain * billion),
	}
}
