package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/PolarBaeJr/proxy-manager/internal/labels"
	"github.com/redis/go-redis/v9"
)

// TestRedisLabelStoreLuaReal exercises the write script against a real
// Redis when REDIS_TEST_ADDR is set (DB 15). It skips rather than clobber
// any existing pmgr:labels keys there.
func TestRedisLabelStoreLuaReal(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set")
	}
	client := redis.NewClient(&redis.Options{Addr: addr, Username: os.Getenv("REDIS_TEST_USERNAME"), Password: os.Getenv("REDIS_TEST_PASSWORD"), DB: 15})
	t.Cleanup(func() { client.Close() })
	ctx := context.Background()
	if existing, err := client.Keys(ctx, "pmgr:labels:*").Result(); err != nil {
		t.Fatal(err)
	} else if len(existing) != 0 {
		t.Skip("DB 15 already has pmgr:labels keys; refusing to clobber")
	}
	t.Cleanup(func() {
		if ks, _ := client.Keys(ctx, "pmgr:labels:*").Result(); len(ks) > 0 {
			client.Del(ctx, ks...)
		}
	})
	sub := client.Subscribe(ctx, labels.RedisChannel)
	t.Cleanup(func() { sub.Close() })
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	s := newRedisLabelStore(client)

	// Wiped (no global version): refused without AllowInit.
	if _, err := s.Write(ctx, labelWrite{Service: "lt", Labels: map[string]string{labelWeight: "2"}}); !errors.Is(err, errLabelsWiped) {
		t.Fatalf("write to a wiped store: %v, want errLabelsWiped", err)
	}
	res, err := s.Write(ctx, labelWrite{Service: "lt", Labels: map[string]string{labelWeight: "2"}, Actor: "alice", AllowInit: true, RequestID: "r1",
		Imported: map[string]map[string]string{"host-a": {labelWeight: "2"}}})
	if err != nil || res.Version != 1 || res.Global != 1 {
		t.Fatalf("first write = %+v, %v", res, err)
	}
	if msg, err := sub.ReceiveMessage(ctx); err != nil || msg.Payload != "lt 1" {
		t.Fatalf("publish = %v, %v", msg, err)
	}
	// Replay beats CAS.
	if res, err := s.Write(ctx, labelWrite{Service: "lt", RequestID: "r1", Labels: map[string]string{labelWeight: "9"}}); err != nil || !res.Replayed || res.Version != 1 {
		t.Fatalf("replay = %+v, %v", res, err)
	}
	var conflict errLabelsVersionConflict
	if _, err := s.Write(ctx, labelWrite{Service: "lt", IfVersion: 0, Labels: map[string]string{}}); !errors.As(err, &conflict) || conflict.Current != 1 {
		t.Fatalf("stale write = %v, want conflict current 1", err)
	}
	res, err = s.Write(ctx, labelWrite{Service: "lt", IfVersion: 1, Labels: map[string]string{labelHealth: "/h"}, Actor: "bob"})
	if err != nil || res.Version != 2 || res.Global != 2 {
		t.Fatalf("second write = %+v, %v", res, err)
	}
	rec, ok, err := s.Get(ctx, "lt")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if rec.Version != 2 || !reflect.DeepEqual(rec.Labels, map[string]string{labelHealth: "/h"}) ||
		!reflect.DeepEqual(rec.Prev, map[string]string{labelWeight: "2"}) || rec.AdoptedBy != "alice" || rec.UpdatedBy != "bob" ||
		rec.Imported["host-a"][labelWeight] != "2" {
		t.Errorf("record = %+v", rec)
	}
	// An all-unset write keeps the service adopted (index + meta) with an
	// empty hash.
	if _, err := s.Write(ctx, labelWrite{Service: "lt", IfVersion: 2, Labels: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	s.Refresh(ctx)
	snap := s.Snapshot()
	if snap == nil || snap.Version != 3 || snap.Wiped || !s.EverLoaded() || !s.RedisOK() {
		t.Fatalf("snapshot = %+v", snap)
	}
	if m, ok := snap.Services["lt"]; !ok || len(m) != 0 {
		t.Errorf("snapshot lt = %v, %v, want adopted with no keys", m, ok)
	}
	// Wipe the counter: the snapshot is kept but marked Wiped, and a reseed
	// restarting the counter at a coincident value still reloads.
	client.Del(ctx, labels.RedisVersion)
	s.Refresh(ctx)
	if snap := s.Snapshot(); snap == nil || !snap.Wiped || len(snap.Services) != 1 {
		t.Fatalf("after wipe snapshot = %+v", snap)
	}
	client.Set(ctx, labels.RedisVersion, 3, 0)
	client.HSet(ctx, labels.RedisServiceKey("lt"), labelWeight, "5")
	s.Refresh(ctx)
	if snap := s.Snapshot(); snap.Wiped || snap.Services["lt"][labelWeight] != "5" {
		t.Errorf("after reseed snapshot = %+v", snap)
	}
}
