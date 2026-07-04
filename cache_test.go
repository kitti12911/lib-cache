package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type user struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var errTestRedis = errors.New("redis failed")

type errorClient struct {
	redis.UniversalClient
	err error
}

type pingCloseErrorClient struct {
	errorClient
}

func (c pingCloseErrorClient) Ping(context.Context) *redis.StatusCmd {
	return redis.NewStatusResult("", c.err)
}

func (c pingCloseErrorClient) Close() error {
	return c.err
}

type getErrorClient struct {
	errorClient
}

func (c getErrorClient) Get(context.Context, string) *redis.StringCmd {
	return redis.NewStringResult("", c.err)
}

type setErrorClient struct {
	errorClient
}

func (c setErrorClient) Set(context.Context, string, interface{}, time.Duration) *redis.StatusCmd {
	return redis.NewStatusResult("", c.err)
}

type delErrorClient struct {
	errorClient
}

func (c delErrorClient) Del(context.Context, ...string) *redis.IntCmd {
	return redis.NewIntResult(0, c.err)
}

type flushErrorClient struct {
	errorClient
}

func (c flushErrorClient) FlushDB(context.Context) *redis.StatusCmd {
	return redis.NewStatusResult("", c.err)
}

type scanErrorClient struct {
	errorClient
}

func (c scanErrorClient) Scan(context.Context, uint64, string, int64) *redis.ScanCmd {
	return redis.NewScanCmdResult(nil, 0, c.err)
}

type stringCodec struct{}

func (stringCodec) Marshal(v any) ([]byte, error) {
	return []byte(fmt.Sprintf("codec:%s", v)), nil
}

func (stringCodec) Unmarshal(data []byte, v any) error {
	*(v.(*string)) = string(data[len("codec:"):])
	return nil
}

func TestTypedCacheSetGetAndDelete(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t, Config{KeyPrefix: "test", DefaultTTL: time.Minute}, false)
	users := Use[user](cache)

	ctx := context.Background()
	expected := user{ID: "u1", Name: "Kitti"}

	require.NoError(t, users.Set(ctx, "users:u1", expected))

	actual, ok, err := users.Get(ctx, "users:u1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, expected, actual)

	require.NoError(t, users.Delete(ctx, "users:u1"))

	_, ok, err = users.Get(ctx, "users:u1")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestNew(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	server := miniredis.RunT(t)

	cache, err := New(ctx, Config{Addr: server.Addr(), KeyPrefix: " test "})
	require.NoError(t, err)
	require.NoError(t, cache.Close())

	_, err = New(ctx, Config{})
	require.EqualError(t, err, "cache: addr is required")

	closedServer := miniredis.RunT(t)
	addr := closedServer.Addr()
	closedServer.Close()

	_, err = New(ctx, Config{
		Addr:         addr,
		DialTimeout:  time.Millisecond,
		ReadTimeout:  time.Millisecond,
		WriteTimeout: time.Millisecond,
	})
	require.ErrorContains(t, err, "cache: ping")
}

func TestCachePingAndCloseErrors(t *testing.T) {
	t.Parallel()

	base := newTestClient(t)
	cache := Wrap(pingCloseErrorClient{errorClient{UniversalClient: base, err: errTestRedis}}, Config{})

	require.ErrorContains(t, cache.Ping(context.Background()), "cache: ping")
	require.ErrorContains(t, cache.Close(), "cache: close")
	require.NoError(t, base.Close())
}

func TestTypedCacheUsesCustomCodec(t *testing.T) {
	t.Parallel()

	server := miniredis.RunT(t)
	cache := Wrap(
		redis.NewClient(&redis.Options{Addr: server.Addr()}),
		Config{KeyPrefix: "test"},
		WithCodec(stringCodec{}),
	)
	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})
	values := Use[string](cache)

	ctx := context.Background()
	require.NoError(t, values.Set(ctx, "value", "ok"))
	stored, err := server.Get("test:value")
	require.NoError(t, err)
	require.Equal(t, "codec:ok", stored)

	value, ok, err := values.Get(ctx, "value")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ok", value)
}

func TestTypedCacheFallsBackToJSONCodecWhenNilCodecIsProvided(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t, Config{KeyPrefix: "test"}, false, WithCodec(nil))
	users := Use[user](cache)

	ctx := context.Background()
	expected := user{ID: "u1"}
	require.NoError(t, users.Set(ctx, "users:u1", expected))

	actual, ok, err := users.Get(ctx, "users:u1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, expected, actual)
}

func TestTypedCacheUsesPerItemTTL(t *testing.T) {
	t.Parallel()

	server := miniredis.RunT(t)
	cache := Wrap(redis.NewClient(&redis.Options{Addr: server.Addr()}), Config{KeyPrefix: "test", DefaultTTL: time.Hour})
	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})
	users := Use[user](cache)

	ctx := context.Background()
	require.NoError(t, users.Set(ctx, "users:u1", user{ID: "u1"}, WithTTL(time.Second)))

	server.FastForward(2 * time.Second)

	_, ok, err := users.Get(ctx, "users:u1")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestTypedCacheGetOrLoadWithoutSingleflight(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t, Config{KeyPrefix: "test", DefaultTTL: time.Minute}, false)
	users := Use[user](cache)

	var calls atomic.Int64
	ctx := context.Background()
	value, err := users.GetOrLoad(ctx, "users:u1", func(context.Context) (user, error) {
		calls.Add(1)
		return user{ID: "u1", Name: "Kitti"}, nil
	})
	require.NoError(t, err)
	require.Equal(t, "u1", value.ID)

	cached, err := users.GetOrLoad(ctx, "users:u1", func(context.Context) (user, error) {
		calls.Add(1)
		return user{ID: "unexpected"}, nil
	})
	require.NoError(t, err)
	require.Equal(t, value, cached)
	require.Equal(t, int64(1), calls.Load())
}

func TestTypedCacheGetOrLoadSingleflight(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t, Config{KeyPrefix: "test", DefaultTTL: time.Minute}, true)
	users := Use[user](cache)

	var calls atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			value, err := users.GetOrLoad(context.Background(), "users:u1", func(context.Context) (user, error) {
				calls.Add(1)
				time.Sleep(10 * time.Millisecond)
				return user{ID: "u1", Name: "Kitti"}, nil
			})
			require.NoError(t, err)
			require.Equal(t, "u1", value.ID)
		}()
	}

	close(start)
	wg.Wait()

	require.Equal(t, int64(1), calls.Load())
}

func TestTypedCacheGetOrLoadSingleflightError(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t, Config{KeyPrefix: "test"}, true)
	users := Use[user](cache)

	value, err := users.GetOrLoad(context.Background(), "users:u1", func(context.Context) (user, error) {
		return user{}, errors.New("loader failed")
	})
	require.ErrorContains(t, err, `cache: singleflight "users:u1"`)
	require.Equal(t, user{}, value)
}

func TestTypedCacheGetOrLoadSingleflightIsolatesPerType(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t, Config{KeyPrefix: "test"}, true)
	strings := Use[string](cache)
	ints := Use[int](cache)

	loadStarted := make(chan struct{})
	releaseLoad := make(chan struct{})
	type stringResult struct {
		value string
		err   error
	}
	stringDone := make(chan stringResult, 1)

	go func() {
		v, err := strings.GetOrLoad(context.Background(), "value", func(context.Context) (string, error) {
			close(loadStarted)
			<-releaseLoad
			return "hello", nil
		})
		stringDone <- stringResult{v, err}
	}()

	<-loadStarted

	type intResult struct {
		value int
		err   error
	}
	intDone := make(chan intResult, 1)
	go func() {
		v, err := ints.GetOrLoad(context.Background(), "value", func(context.Context) (int, error) {
			return 1, nil
		})
		intDone <- intResult{v, err}
	}()

	time.Sleep(10 * time.Millisecond)
	close(releaseLoad)

	gotString := <-stringDone
	gotInt := <-intDone
	require.NoError(t, gotString.err)
	require.Equal(t, "hello", gotString.value)
	require.NoError(t, gotInt.err)
	require.Equal(t, 1, gotInt.value)
}

func TestTypedCacheGetOrLoadPropagatesContextCancellation(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t, Config{KeyPrefix: "test"}, true)
	users := Use[user](cache)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := users.GetOrLoad(ctx, "user:u1", func(ctx context.Context) (user, error) {
		return user{}, ctx.Err()
	})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestTypedCacheGetOrLoadReturnsCacheGetError(t *testing.T) {
	t.Parallel()

	base := newTestClient(t)
	cache := Wrap(getErrorClient{errorClient{UniversalClient: base, err: errTestRedis}}, Config{})
	users := Use[user](cache)

	value, err := users.GetOrLoad(context.Background(), "users:u1", func(context.Context) (user, error) {
		return user{ID: "u1"}, nil
	})
	require.ErrorContains(t, err, `cache: get "users:u1"`)
	require.Equal(t, user{}, value)
	require.NoError(t, base.Close())
}

func TestTypedCacheGetOrLoadReturnsLoaderError(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t, Config{KeyPrefix: "test"}, false)
	users := Use[user](cache)

	value, err := users.GetOrLoad(context.Background(), "users:u1", func(context.Context) (user, error) {
		return user{}, errors.New("loader failed")
	})
	require.ErrorContains(t, err, `cache: load "users:u1"`)
	require.Equal(t, user{}, value)
}

func TestTypedCacheGetOrLoadReturnsSetError(t *testing.T) {
	t.Parallel()

	base := newTestClient(t)
	cache := Wrap(setErrorClient{errorClient{UniversalClient: base, err: errTestRedis}}, Config{})
	users := Use[user](cache)

	value, err := users.GetOrLoad(context.Background(), "users:u1", func(context.Context) (user, error) {
		return user{ID: "u1"}, nil
	})
	require.ErrorContains(t, err, `cache: set "users:u1"`)
	require.Equal(t, user{}, value)
	require.NoError(t, base.Close())
}

func TestTypedCacheGetErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cache := newTestCache(t, Config{KeyPrefix: "test"}, false)
	require.NoError(t, cache.client.Set(ctx, cache.key("bad-json"), "{", 0).Err())

	_, ok, err := Use[user](cache).Get(ctx, "bad-json")
	require.ErrorContains(t, err, `cache: unmarshal "bad-json"`)
	require.False(t, ok)

	base := newTestClient(t)
	errorCache := Wrap(getErrorClient{errorClient{UniversalClient: base, err: errTestRedis}}, Config{})
	_, ok, err = Use[user](errorCache).Get(ctx, "users:u1")
	require.ErrorContains(t, err, `cache: get "users:u1"`)
	require.False(t, ok)
	require.NoError(t, base.Close())
}

func TestTypedCacheSetErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cache := newTestCache(t, Config{KeyPrefix: "test"}, false)
	require.ErrorContains(t, Use[chan int](cache).Set(ctx, "bad", make(chan int)), `cache: marshal "bad"`)

	base := newTestClient(t)
	errorCache := Wrap(setErrorClient{errorClient{UniversalClient: base, err: errTestRedis}}, Config{})
	require.ErrorContains(t, Use[user](errorCache).Set(ctx, "users:u1", user{ID: "u1"}), `cache: set "users:u1"`)
	require.NoError(t, base.Close())
}

func TestClearOnlyDeletesNamespacedKeys(t *testing.T) {
	t.Parallel()

	server := miniredis.RunT(t)
	cache := Wrap(redis.NewClient(&redis.Options{Addr: server.Addr()}), Config{KeyPrefix: "app"})
	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})
	users := Use[user](cache)

	ctx := context.Background()
	require.NoError(t, users.Set(ctx, "users:u1", user{ID: "u1"}))
	require.NoError(t, server.Set("other:users:u1", "keep"))

	require.NoError(t, cache.Clear(ctx))

	_, ok, err := users.Get(ctx, "users:u1")
	require.NoError(t, err)
	require.False(t, ok)
	require.True(t, server.Exists("other:users:u1"))
}

func TestClearFlushesDatabaseWithoutKeyPrefix(t *testing.T) {
	t.Parallel()

	server := miniredis.RunT(t)
	cache := Wrap(redis.NewClient(&redis.Options{Addr: server.Addr()}), Config{})
	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})

	require.NoError(t, server.Set("users:u1", "delete"))
	require.NoError(t, server.Set("other:users:u1", "delete"))

	require.NoError(t, cache.Clear(context.Background()))

	require.False(t, server.Exists("users:u1"))
	require.False(t, server.Exists("other:users:u1"))
}

func TestClearDeletesFullBatches(t *testing.T) {
	t.Parallel()

	server := miniredis.RunT(t)
	cache := Wrap(redis.NewClient(&redis.Options{Addr: server.Addr()}), Config{KeyPrefix: "app"})
	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})

	for i := range 1000 {
		require.NoError(t, server.Set(fmt.Sprintf("app:key:%d", i), "delete"))
	}

	require.NoError(t, cache.Clear(context.Background()))
	require.Empty(t, server.Keys())
}

func TestCacheDeleteErrors(t *testing.T) {
	t.Parallel()

	base := newTestClient(t)
	cache := Wrap(delErrorClient{errorClient{UniversalClient: base, err: errTestRedis}}, Config{})

	require.NoError(t, cache.Delete(context.Background()))
	require.ErrorContains(t, cache.Delete(context.Background(), "users:u1"), "cache: delete")
	require.NoError(t, base.Close())
}

func TestCacheClearErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	base := newTestClient(t)
	require.ErrorContains(t, Wrap(flushErrorClient{errorClient{UniversalClient: base, err: errTestRedis}}, Config{}).Clear(ctx), "cache: clear database")
	require.NoError(t, base.Close())

	base = newTestClient(t)
	require.ErrorContains(t, Wrap(scanErrorClient{errorClient{UniversalClient: base, err: errTestRedis}}, Config{KeyPrefix: "app"}).Clear(ctx), "cache: scan")
	require.NoError(t, base.Close())

	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	require.NoError(t, server.Set("app:users:u1", "delete"))
	require.ErrorContains(
		t,
		Wrap(delErrorClient{errorClient{UniversalClient: redisClient, err: errTestRedis}}, Config{KeyPrefix: "app"}).Clear(ctx),
		"cache: clear batch",
	)
	require.NoError(t, redisClient.Close())

	server = miniredis.RunT(t)
	redisClient = redis.NewClient(&redis.Options{Addr: server.Addr()})
	for i := range 1000 {
		require.NoError(t, server.Set(fmt.Sprintf("app:batch:%d", i), "delete"))
	}
	require.ErrorContains(
		t,
		Wrap(delErrorClient{errorClient{UniversalClient: redisClient, err: errTestRedis}}, Config{KeyPrefix: "app"}).Clear(ctx),
		"cache: clear batch",
	)
	require.NoError(t, redisClient.Close())
}

func TestJSONCodecErrors(t *testing.T) {
	t.Parallel()

	_, err := JSONCodec{}.Marshal(make(chan int))
	require.ErrorContains(t, err, "cache: json marshal")

	var value user
	require.ErrorContains(t, JSONCodec{}.Unmarshal([]byte("{"), &value), "cache: json unmarshal")
}

func newTestCache(t *testing.T, cfg Config, singleflight bool, opts ...Option) *Cache {
	t.Helper()

	server := miniredis.RunT(t)
	wrapOptions := append([]Option{WithSingleflight(singleflight)}, opts...)
	cache := Wrap(redis.NewClient(&redis.Options{Addr: server.Addr()}), cfg, wrapOptions...)
	t.Cleanup(func() {
		require.NoError(t, cache.Close())
	})

	return cache
}

func newTestClient(t *testing.T) *redis.Client {
	t.Helper()

	server := miniredis.RunT(t)
	return redis.NewClient(&redis.Options{Addr: server.Addr()})
}

func TestManagerNamespaceIsolatesKeys(t *testing.T) {
	server := miniredis.RunT(t)

	manager, err := NewManager(context.Background(), Config{Addr: server.Addr(), KeyPrefix: "svc"}, WithSingleflight(true))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.Close())
	})

	skill := Use[string](manager.Namespace("skill"))
	part := Use[string](manager.Namespace("part"))

	require.NoError(t, skill.Set(context.Background(), "k", "s"))
	require.NoError(t, part.Set(context.Background(), "k", "p"))
	require.True(t, server.Exists("svc:skill:k"))
	require.True(t, server.Exists("svc:part:k"))

	require.NoError(t, manager.Namespace("skill").Clear(context.Background()))
	require.False(t, server.Exists("svc:skill:k"))
	require.True(t, server.Exists("svc:part:k"))
}

func TestNewManagerRequiresAddr(t *testing.T) {
	_, err := NewManager(context.Background(), Config{Addr: "  "})
	require.Error(t, err)
	require.Contains(t, err.Error(), "addr is required")
}

func TestNewManagerPingFailure(t *testing.T) {
	server := miniredis.RunT(t)
	addr := server.Addr()
	server.Close() // connection refused -> ping fails, client is closed

	_, err := NewManager(context.Background(), Config{Addr: addr})
	require.Error(t, err)
}

func TestManagerNamespaceWithoutBasePrefix(t *testing.T) {
	server := miniredis.RunT(t)

	manager, err := NewManager(context.Background(), Config{Addr: server.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.Close() })

	// no base prefix: the namespace becomes the whole prefix
	skill := Use[string](manager.Namespace("skill"))
	require.NoError(t, skill.Set(context.Background(), "k", "v"))
	require.True(t, server.Exists("skill:k"))

	// empty namespace on an empty base: keys stay unprefixed
	bare := Use[string](manager.Namespace("  "))
	require.NoError(t, bare.Set(context.Background(), "k2", "v"))
	require.True(t, server.Exists("k2"))
}

func TestManagerNamespaceEmptyNameKeepsBasePrefix(t *testing.T) {
	server := miniredis.RunT(t)

	manager, err := NewManager(context.Background(), Config{Addr: server.Addr(), KeyPrefix: "svc"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.Close() })

	c := Use[string](manager.Namespace(""))
	require.NoError(t, c.Set(context.Background(), "k", "v"))
	require.True(t, server.Exists("svc:k"))
}

func TestManagerCloseTwiceReturnsError(t *testing.T) {
	server := miniredis.RunT(t)

	manager, err := NewManager(context.Background(), Config{Addr: server.Addr()})
	require.NoError(t, err)

	require.NoError(t, manager.Close())
	err = manager.Close()
	require.Error(t, err)
	require.Contains(t, err.Error(), "close manager")
}
