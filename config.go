package cache

import "time"

type Config struct {
	Addr         string        `mapstructure:"addr"          env:"CACHE_ADDR"           validate:"required"`
	Username     string        `mapstructure:"username"      env:"CACHE_USERNAME"`
	Password     string        `mapstructure:"password"      env:"CACHE_PASSWORD"`
	DB           int           `mapstructure:"db"            env:"CACHE_DB"`
	KeyPrefix    string        `mapstructure:"key_prefix"    env:"CACHE_KEY_PREFIX"`
	DefaultTTL   time.Duration `mapstructure:"default_ttl"   env:"CACHE_DEFAULT_TTL"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout"  env:"CACHE_DIAL_TIMEOUT"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"  env:"CACHE_READ_TIMEOUT"`
	WriteTimeout time.Duration `mapstructure:"write_timeout" env:"CACHE_WRITE_TIMEOUT"`
	PoolSize     int           `mapstructure:"pool_size"     env:"CACHE_POOL_SIZE"`
}
