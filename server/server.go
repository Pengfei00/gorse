// Copyright 2020 gorse Project Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/emicklei/go-restful/v3"
	"github.com/juju/errors"
	"github.com/samber/lo"
	"github.com/scylladb/go-set/strset"
	"github.com/zhenghaoz/gorse/base"
	"github.com/zhenghaoz/gorse/config"
	"github.com/zhenghaoz/gorse/protocol"
	"github.com/zhenghaoz/gorse/storage/cache"
	"github.com/zhenghaoz/gorse/storage/data"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"math"
	"math/rand"
	"sync"
	"time"
)

// Server manages states of a server node.
type Server struct {
	RestServer
	cachePath    string
	dataPath     string
	masterClient protocol.MasterClient
	serverName   string
	masterHost   string
	masterPort   int
	testMode     bool
	cacheFile    string
}

// NewServer creates a server node.
func NewServer(masterHost string, masterPort int, serverHost string, serverPort int, cacheFile string) *Server {
	s := &Server{
		masterHost: masterHost,
		masterPort: masterPort,
		cacheFile:  cacheFile,
		RestServer: RestServer{
			DataClient:  &data.NoDatabase{},
			CacheClient: &cache.NoDatabase{},
			GorseConfig: config.GetDefaultConfig(),
			HttpHost:    serverHost,
			HttpPort:    serverPort,
			WebService:  new(restful.WebService),
		},
	}
	s.RestServer.PopularItemsCache = NewPopularItemsCache(&s.RestServer)
	s.RestServer.HiddenItemsCache = NewHiddenItemsCache(&s.RestServer)
	return s
}

// Serve starts a server node.
func (s *Server) Serve() {
	rand.Seed(time.Now().UTC().UnixNano())
	// open local store
	state, err := LoadLocalCache(s.cacheFile)
	if err != nil {
		base.Logger().Error("failed to connect local store", zap.Error(err),
			zap.String("path", state.path))
	}
	if state.ServerName == "" {
		state.ServerName = base.GetRandomName(0)
		err = state.WriteLocalCache()
		if err != nil {
			base.Logger().Fatal("failed to write meta", zap.Error(err))
		}
	}
	s.serverName = state.ServerName
	base.Logger().Info("start server",
		zap.String("server_name", s.serverName),
		zap.String("server_host", s.HttpHost),
		zap.Int("server_port", s.HttpPort),
		zap.String("master_host", s.masterHost),
		zap.Int("master_port", s.masterPort))

	// connect to master
	conn, err := grpc.Dial(fmt.Sprintf("%v:%v", s.masterHost, s.masterPort), grpc.WithInsecure())
	if err != nil {
		base.Logger().Fatal("failed to connect master", zap.Error(err))
	}
	s.masterClient = protocol.NewMasterClient(conn)

	go s.Sync()
	s.StartHttpServer()
}

// Sync this server to the master.
func (s *Server) Sync() {
	defer base.CheckPanic()
	base.Logger().Info("start meta sync", zap.Duration("meta_timeout", s.GorseConfig.Master.MetaTimeout))
	for {
		var meta *protocol.Meta
		var err error
		if meta, err = s.masterClient.GetMeta(context.Background(),
			&protocol.NodeInfo{
				NodeType: protocol.NodeType_ServerNode,
				NodeName: s.serverName,
				HttpPort: int64(s.HttpPort),
			}); err != nil {
			base.Logger().Error("failed to get meta", zap.Error(err))
			goto sleep
		}

		// load master config
		err = json.Unmarshal([]byte(meta.Config), &s.GorseConfig)
		if err != nil {
			base.Logger().Error("failed to parse master config", zap.Error(err))
			goto sleep
		}

		// connect to data store
		if s.dataPath != s.GorseConfig.Database.DataStore {
			base.Logger().Info("connect data store", zap.String("database", s.GorseConfig.Database.DataStore))
			if s.DataClient, err = data.Open(s.GorseConfig.Database.DataStore); err != nil {
				base.Logger().Error("failed to connect data store", zap.Error(err))
				goto sleep
			}
			s.dataPath = s.GorseConfig.Database.DataStore
		}

		// connect to cache store
		if s.cachePath != s.GorseConfig.Database.CacheStore {
			base.Logger().Info("connect cache store", zap.String("database", s.GorseConfig.Database.CacheStore))
			if s.CacheClient, err = cache.Open(s.GorseConfig.Database.CacheStore); err != nil {
				base.Logger().Error("failed to connect cache store", zap.Error(err))
				goto sleep
			}
			s.cachePath = s.GorseConfig.Database.CacheStore
		}

	sleep:
		if s.testMode {
			return
		}
		time.Sleep(s.GorseConfig.Master.MetaTimeout)
	}
}

type PopularItemsCache struct {
	mu     sync.RWMutex
	scores map[string]float64
	server *RestServer
	test   bool
}

func NewPopularItemsCache(s *RestServer) *PopularItemsCache {
	sc := &PopularItemsCache{
		server: s,
		scores: make(map[string]float64),
	}
	go func() {
		for {
			sc.sync()
			base.Logger().Debug("refresh server side popular items cache", zap.String("cache_expire", s.GorseConfig.Server.CacheExpire.String()))
			time.Sleep(s.GorseConfig.Server.CacheExpire)
		}
	}()
	return sc
}

func newPopularItemsCacheForTest(s *RestServer) *PopularItemsCache {
	sc := &PopularItemsCache{
		server: s,
		scores: make(map[string]float64),
		test:   true,
	}
	return sc
}

func (sc *PopularItemsCache) sync() {
	// load popular items
	items, err := sc.server.CacheClient.GetSorted(cache.Key(cache.PopularItems), 0, -1)
	if err != nil {
		if !errors.IsNotAssigned(err) {
			base.Logger().Error("failed to get popular items", zap.Error(err))
		}
		return
	}
	scores := make(map[string]float64)
	for _, item := range items {
		scores[item.Id] = item.Score
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.scores = scores
}

func (sc *PopularItemsCache) GetSortedScore(member string) float64 {
	if sc.test {
		sc.sync()
	}
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	score, _ := sc.scores[member]
	return score
}

type HiddenItemsCache struct {
	server      *RestServer
	mu          sync.RWMutex
	hiddenItems *strset.Set
	updateTime  time.Time
	test        bool
}

func NewHiddenItemsCache(s *RestServer) *HiddenItemsCache {
	hc := &HiddenItemsCache{
		server:      s,
		hiddenItems: strset.New(),
	}
	go func() {
		for {
			hc.sync()
			base.Logger().Debug("refresh server side hidden items cache", zap.String("cache_expire", s.GorseConfig.Server.CacheExpire.String()))
			time.Sleep(hc.server.GorseConfig.Server.CacheExpire)
		}
	}()
	return hc
}

func (hc *HiddenItemsCache) sync() {
	ts := time.Now()
	// load hidden items
	score, err := hc.server.CacheClient.GetSortedByScore(cache.HiddenItemsV2, math.Inf(-1), float64(ts.Unix()))
	if err != nil {
		if !errors.IsNotAssigned(err) {
			base.Logger().Error("failed to load hidden items", zap.Error(err))
		}
		return
	}
	if len(score) > 0 {
		fmt.Println(score)
	}
	hiddenItems := strset.New(cache.RemoveScores(score)...)
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.hiddenItems = hiddenItems
	hc.updateTime = ts
}

func (hc *HiddenItemsCache) IsHidden(members []string) ([]bool, error) {
	hc.mu.RLock()
	hiddenItems := hc.hiddenItems
	updateTime := hc.updateTime
	hc.mu.RUnlock()
	// load hidden items
	score, err := hc.server.CacheClient.GetSortedByScore(cache.HiddenItemsV2, float64(updateTime.Unix()), float64(time.Now().Unix()))
	if err != nil {
		return nil, errors.Trace(err)
	}
	deltaHiddenItems := strset.New(cache.RemoveScores(score)...)
	return lo.Map(members, func(t string, i int) bool {
		return hiddenItems.Has(t) || deltaHiddenItems.Has(t)
	}), nil
}
