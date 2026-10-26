// Package trooperredis instruments redis/go-redis v9 via redisotel with TaskTrooper defaults:
// db.statement (command arguments) is NOT recorded, db.system=redis and server.address are set,
// and pool/command metrics are enabled.
//
//	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
//	if err := trooperredis.Instrument(rdb); err != nil { /* telemetry only; safe to ignore */ }
package trooperredis
