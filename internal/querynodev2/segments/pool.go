// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package segments

import (
	"context"
	"math"
	"runtime"
	"sync"

	"go.uber.org/atomic"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/pkg/config"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/conc"
	"github.com/milvus-io/milvus/pkg/util/hardware"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
)

var (
	// Use separate pool for search/query
	// and other operations (insert/delete/statistics/etc.)
	// since in concurrent situation, there operation may block each other in high payload

	sqp      atomic.Pointer[conc.Pool[any]]
	sqOnce   sync.Once
	dp       atomic.Pointer[conc.Pool[any]]
	dynOnce  sync.Once
	loadPool atomic.Pointer[conc.Pool[any]]
	loadOnce sync.Once
)

// initSQPool initialize
func initSQPool() {
	sqOnce.Do(func() {
		pt := paramtable.Get()
		initPoolSize := int(math.Ceil(pt.QueryNodeCfg.MaxReadConcurrency.GetAsFloat() * pt.QueryNodeCfg.CGOPoolSizeRatio.GetAsFloat()))
		pool := conc.NewPool[any](
			initPoolSize,
			conc.WithPreAlloc(false), // pre alloc must be false to resize pool dynamically, use warmup to alloc worker here
			conc.WithDisablePurge(true),
		)
		conc.WarmupPool(pool, runtime.LockOSThread)
		sqp.Store(pool)

		pt.Watch(pt.QueryNodeCfg.MaxReadConcurrency.Key, config.NewHandler("qn.sqpool.maxconc", ResizeSQPool))
		pt.Watch(pt.QueryNodeCfg.CGOPoolSizeRatio.Key, config.NewHandler("qn.sqpool.cgopoolratio", ResizeSQPool))
	})
}

func initDynamicPool() {
	dynOnce.Do(func() {
		pool := conc.NewPool[any](
			hardware.GetCPUNum(),
			conc.WithPreAlloc(false),
			conc.WithDisablePurge(false),
			conc.WithPreHandler(runtime.LockOSThread), // lock os thread for cgo thread disposal
		)

		dp.Store(pool)
	})
}

func initLoadPool() {
	loadOnce.Do(func() {
		pt := paramtable.Get()
		pool := conc.NewPool[any](
			hardware.GetCPUNum()*pt.CommonCfg.MiddlePriorityThreadCoreCoefficient.GetAsInt(),
			conc.WithPreAlloc(false),
			conc.WithDisablePurge(false),
			conc.WithPreHandler(runtime.LockOSThread), // lock os thread for cgo thread disposal
		)

		loadPool.Store(pool)

		pt.Watch(pt.CommonCfg.MiddlePriorityThreadCoreCoefficient.Key, config.NewHandler("qn.loadpool.middlepriority", ResizeLoadPool))
	})
}

// GetSQPool returns the singleton pool instance for search/query operations.
func GetSQPool() *conc.Pool[any] {
	initSQPool()
	return sqp.Load()
}

// GetDynamicPool returns the singleton pool for dynamic cgo operations.
func GetDynamicPool() *conc.Pool[any] {
	initDynamicPool()
	return dp.Load()
}

func GetLoadPool() *conc.Pool[any] {
	initLoadPool()
	return loadPool.Load()
}

func ResizeSQPool(evt *config.Event) {
	if evt.HasUpdated {
		pt := paramtable.Get()
		newSize := int(math.Ceil(pt.QueryNodeCfg.MaxReadConcurrency.GetAsFloat() * pt.QueryNodeCfg.CGOPoolSizeRatio.GetAsFloat()))
		pool := GetSQPool()
		resizePool(pool, newSize, "SQPool")
		conc.WarmupPool(pool, runtime.LockOSThread)
	}
}

func ResizeLoadPool(evt *config.Event) {
	if evt.HasUpdated {
		pt := paramtable.Get()
		newSize := hardware.GetCPUNum() * pt.CommonCfg.MiddlePriorityThreadCoreCoefficient.GetAsInt()
		resizePool(GetLoadPool(), newSize, "LoadPool")
	}
}

func resizePool(pool *conc.Pool[any], newSize int, tag string) {
	log := log.Ctx(context.Background()).
		With(
			zap.String("poolTag", tag),
			zap.Int("newSize", newSize),
		)

	if newSize <= 0 {
		log.Warn("cannot set pool size to non-positive value")
		return
	}

	err := pool.Resize(newSize)
	if err != nil {
		log.Warn("failed to resize pool", zap.Error(err))
		return
	}
	log.Info("pool resize successfully")
}
