package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

type Option func(*options)

type options struct {
	codec        Codec
	singleflight bool
}

type ItemOption func(*itemOptions)

type itemOptions struct {
	ttl time.Duration
}

type Cache struct {
	client       redis.UniversalClient
	codec        Codec
	keyPrefix    string
	defaultTTL   time.Duration
	singleflight bool
	group        singleflight.Group
}

type Typed[T any] struct {
	cache *Cache
}

func WithCodec(codec Codec) Option {
	return func(opts *options) {
		if codec != nil {
			opts.codec = codec
		}
	}
}

func WithSingleflight(enabled bool) Option {
	return func(opts *options) {
		opts.singleflight = enabled
	}
}

func WithTTL(ttl time.Duration) ItemOption {
	return func(opts *itemOptions) {
		opts.ttl = ttl
	}
}

func New(ctx context.Context, cfg Config, opts ...Option) (*Cache, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("cache: addr is required")
	}

	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Username:     cfg.Username,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		PoolSize:     cfg.PoolSize,
	})

	cache := Wrap(client, cfg, opts...)
	if err := cache.Ping(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}

	return cache, nil
}

func Wrap(client redis.UniversalClient, cfg Config, opts ...Option) *Cache {
	cfgOptions := options{
		codec: JSONCodec{},
	}
	for _, opt := range opts {
		opt(&cfgOptions)
	}

	return &Cache{
		client:       client,
		codec:        cfgOptions.codec,
		keyPrefix:    strings.TrimSpace(cfg.KeyPrefix),
		defaultTTL:   cfg.DefaultTTL,
		singleflight: cfgOptions.singleflight,
	}
}

func Use[T any](cache *Cache) *Typed[T] {
	return &Typed[T]{cache: cache}
}

func (c *Cache) Ping(ctx context.Context) error {
	if err := c.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cache: ping: %w", err)
	}
	return nil
}

func (c *Cache) Close() error {
	if err := c.client.Close(); err != nil {
		return fmt.Errorf("cache: close: %w", err)
	}
	return nil
}

func (c *Cache) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}

	namespaced := make([]string, 0, len(keys))
	for _, key := range keys {
		namespaced = append(namespaced, c.key(key))
	}

	if err := c.client.Del(ctx, namespaced...).Err(); err != nil {
		return fmt.Errorf("cache: delete: %w", err)
	}
	return nil
}

func (c *Cache) Clear(ctx context.Context) error {
	if c.keyPrefix == "" {
		if err := c.client.FlushDB(ctx).Err(); err != nil {
			return fmt.Errorf("cache: clear database: %w", err)
		}
		return nil
	}

	iter := c.client.Scan(ctx, 0, c.key("*"), 1000).Iterator()
	var batch []string
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) < 1000 {
			continue
		}
		if err := c.client.Del(ctx, batch...).Err(); err != nil {
			return fmt.Errorf("cache: clear batch: %w", err)
		}
		batch = batch[:0]
	}

	if err := iter.Err(); err != nil {
		return fmt.Errorf("cache: scan: %w", err)
	}

	if len(batch) == 0 {
		return nil
	}
	if err := c.client.Del(ctx, batch...).Err(); err != nil {
		return fmt.Errorf("cache: clear batch: %w", err)
	}
	return nil
}

func (c *Cache) key(key string) string {
	if c.keyPrefix == "" {
		return key
	}
	return c.keyPrefix + ":" + key
}

func (c *Typed[T]) Get(ctx context.Context, key string) (value T, ok bool, err error) {
	var zero T
	data, err := c.cache.client.Get(ctx, c.cache.key(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("cache: get %q: %w", key, err)
	}

	if err := c.cache.codec.Unmarshal(data, &value); err != nil {
		return zero, false, fmt.Errorf("cache: unmarshal %q: %w", key, err)
	}
	return value, true, nil
}

func (c *Typed[T]) Set(ctx context.Context, key string, value T, opts ...ItemOption) error {
	writeOptions := c.itemOptions(opts...)
	data, err := c.cache.codec.Marshal(value)
	if err != nil {
		return fmt.Errorf("cache: marshal %q: %w", key, err)
	}

	if err := c.cache.client.Set(ctx, c.cache.key(key), data, writeOptions.ttl).Err(); err != nil {
		return fmt.Errorf("cache: set %q: %w", key, err)
	}
	return nil
}

func (c *Typed[T]) GetOrLoad(
	ctx context.Context,
	key string,
	load func(context.Context) (T, error),
	opts ...ItemOption,
) (T, error) {
	if value, ok, err := c.Get(ctx, key); ok || err != nil {
		return value, err
	}

	if !c.cache.singleflight {
		return c.loadAndSet(ctx, key, load, opts...)
	}

	var zero T
	groupKey := fmt.Sprintf("%T:%s", zero, c.cache.key(key))
	value, err, _ := c.cache.group.Do(groupKey, func() (any, error) {
		return c.loadAndSet(ctx, key, load, opts...)
	})
	if err != nil {
		return zero, fmt.Errorf("cache: singleflight %q: %w", key, err)
	}
	return value.(T), nil
}

func (c *Typed[T]) Delete(ctx context.Context, keys ...string) error {
	return c.cache.Delete(ctx, keys...)
}

func (c *Typed[T]) loadAndSet(
	ctx context.Context,
	key string,
	load func(context.Context) (T, error),
	opts ...ItemOption,
) (T, error) {
	value, err := load(ctx)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("cache: load %q: %w", key, err)
	}

	if err := c.Set(ctx, key, value, opts...); err != nil {
		var zero T
		return zero, err
	}
	return value, nil
}

func (c *Typed[T]) itemOptions(opts ...ItemOption) itemOptions {
	writeOptions := itemOptions{
		ttl: c.cache.defaultTTL,
	}
	for _, opt := range opts {
		opt(&writeOptions)
	}
	return writeOptions
}
