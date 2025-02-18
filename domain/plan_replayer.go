// Copyright 2021 PingCAP, Inc.
//
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

package domain

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/sqlexec"
	"go.uber.org/zap"
)

// dumpFileGcChecker is used to gc dump file in circle
// For now it is used by `plan replayer` and `trace plan` statement
type dumpFileGcChecker struct {
	sync.Mutex
	gcLease            time.Duration
	paths              []string
	planReplayerHandle *planReplayerHandle
}

// GetPlanReplayerDirName returns plan replayer directory path.
// The path is related to the process id.
func GetPlanReplayerDirName() string {
	tidbLogDir := filepath.Dir(config.GetGlobalConfig().Log.File.Filename)
	return filepath.Join(tidbLogDir, "replayer")
}

func parseType(s string) string {
	return strings.Split(s, "_")[0]
}

func parseTime(s string) (time.Time, error) {
	startIdx := strings.LastIndex(s, "_")
	if startIdx == -1 {
		return time.Time{}, errors.New("failed to parse the file :" + s)
	}
	endIdx := strings.LastIndex(s, ".")
	if endIdx == -1 || endIdx <= startIdx+1 {
		return time.Time{}, errors.New("failed to parse the file :" + s)
	}
	i, err := strconv.ParseInt(s[startIdx+1:endIdx], 10, 64)
	if err != nil {
		return time.Time{}, errors.New("failed to parse the file :" + s)
	}
	return time.Unix(0, i), nil
}

func (p *dumpFileGcChecker) gcDumpFiles(t time.Duration) {
	p.Lock()
	defer p.Unlock()
	for _, path := range p.paths {
		p.gcDumpFilesByPath(path, t)
	}
}

func (p *dumpFileGcChecker) setupPlanReplayerHandle(handle *planReplayerHandle) {
	p.planReplayerHandle = handle
}

func (p *dumpFileGcChecker) gcDumpFilesByPath(path string, t time.Duration) {
	files, err := ioutil.ReadDir(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logutil.BgLogger().Warn("[dumpFileGcChecker] open plan replayer directory failed", zap.Error(err))
		}
	}

	gcTime := time.Now().Add(-t)
	for _, f := range files {
		fileName := f.Name()
		createTime, err := parseTime(fileName)
		if err != nil {
			logutil.BgLogger().Error("[dumpFileGcChecker] parseTime failed", zap.Error(err), zap.String("filename", fileName))
			continue
		}
		isPlanReplayer := parseType(fileName) == "replayer"
		if !createTime.After(gcTime) {
			err := os.Remove(filepath.Join(path, f.Name()))
			if err != nil {
				logutil.BgLogger().Warn("[dumpFileGcChecker] remove file failed", zap.Error(err), zap.String("filename", fileName))
				continue
			}
			logutil.BgLogger().Info("dumpFileGcChecker successful", zap.String("filename", fileName))
			if isPlanReplayer && p.planReplayerHandle != nil {
				p.planReplayerHandle.deletePlanReplayerStatus(context.Background(), fileName)
			}
		}
	}
}

type planReplayerHandle struct {
	sync.Mutex
	sctx sessionctx.Context
}

// DeletePlanReplayerStatus delete  mysql.plan_replayer_status record
func (h *planReplayerHandle) deletePlanReplayerStatus(ctx context.Context, token string) {
	ctx1 := kv.WithInternalSourceType(ctx, kv.InternalTxnStats)
	h.Lock()
	defer h.Unlock()
	exec := h.sctx.(sqlexec.SQLExecutor)
	_, err := exec.ExecuteInternal(ctx1, fmt.Sprintf("delete from mysql.plan_replayer_status where token = %v", token))
	if err != nil {
		logutil.BgLogger().Warn("delete mysql.plan_replayer_status record failed", zap.String("token", token), zap.Error(err))
	}
}

// InsertPlanReplayerStatus insert mysql.plan_replayer_status record
func (h *planReplayerHandle) InsertPlanReplayerStatus(ctx context.Context, records []PlanReplayerStatusRecord) {
	ctx1 := kv.WithInternalSourceType(ctx, kv.InternalTxnStats)
	var instance string
	serverInfo, err := infosync.GetServerInfo()
	if err != nil {
		logutil.BgLogger().Error("failed to get server info", zap.Error(err))
		instance = "unknown"
	} else {
		instance = fmt.Sprintf("%s:%d", serverInfo.IP, serverInfo.Port)
	}
	for _, record := range records {
		if !record.Internal {
			if len(record.FailedReason) > 0 {
				h.insertExternalPlanReplayerErrorStatusRecord(ctx1, instance, record)
			} else {
				h.insertExternalPlanReplayerSuccessStatusRecord(ctx1, instance, record)
			}
		}
	}
}

func (h *planReplayerHandle) insertExternalPlanReplayerErrorStatusRecord(ctx context.Context, instance string, record PlanReplayerStatusRecord) {
	h.Lock()
	defer h.Unlock()
	exec := h.sctx.(sqlexec.SQLExecutor)
	_, err := exec.ExecuteInternal(ctx, fmt.Sprintf(
		"insert into mysql.plan_replayer_status (origin_sql, fail_reason, instance) values ('%s','%s','%s')",
		record.OriginSQL, record.FailedReason, instance))
	if err != nil {
		logutil.BgLogger().Warn("insert mysql.plan_replayer_status record failed",
			zap.Error(err))
	}
}

func (h *planReplayerHandle) insertExternalPlanReplayerSuccessStatusRecord(ctx context.Context, instance string, record PlanReplayerStatusRecord) {
	h.Lock()
	defer h.Unlock()
	exec := h.sctx.(sqlexec.SQLExecutor)
	_, err := exec.ExecuteInternal(ctx, fmt.Sprintf(
		"insert into mysql.plan_replayer_status (origin_sql, token, instance) values ('%s','%s','%s')",
		record.OriginSQL, record.Token, instance))
	if err != nil {
		logutil.BgLogger().Warn("insert mysql.plan_replayer_status record failed",
			zap.Error(err))
	}
}

// PlanReplayerStatusRecord indicates record in mysql.plan_replayer_status
type PlanReplayerStatusRecord struct {
	Internal     bool
	OriginSQL    string
	Token        string
	FailedReason string
}
