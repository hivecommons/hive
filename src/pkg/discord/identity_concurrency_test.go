package discord

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestIdentityAndGuildCachesShareMutex(t *testing.T) {
	const readers = 32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"bot-user","guild_id":"guild-1"}`))
	}))
	defer server.Close()
	bot := newTestBot(server, "status")
	bot.rememberGuild("thread-1", "guild-1")
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			bot.rememberGuild("thread-1", "guild-1")
			if id, err := bot.selfID(); err != nil || id != "bot-user" {
				t.Errorf("cached self identity=%q err=%v", id, err)
			}
			if url, err := bot.ThreadURL("thread-1"); err != nil || url != "https://discord.com/channels/guild-1/thread-1" {
				t.Errorf("cached guild URL=%q err=%v", url, err)
			}
		}()
	}
	close(start)
	workers.Wait()
}
