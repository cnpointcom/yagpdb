package bot

import (
	"github.com/jonas747/discordgo"
	"github.com/jonas747/yagpdb/bot/eventsystem"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/common/pubsub"
	"github.com/mediocregopher/radix.v3"
	log "github.com/sirupsen/logrus"
	"sync"
)

var (
	waitingGuildsMU sync.Mutex
	waitingGuilds   = make(map[int64]bool)
	waitingReadies  []int

	botStartedFired = new(int32)
)

func HandleReady(data *eventsystem.EventData) {
	evt := data.Ready()

	waitingGuildsMU.Lock()
	for i, v := range waitingReadies {
		if ContextSession(data.Context()).ShardID == v {
			waitingReadies = append(waitingReadies[:i], waitingReadies[i+1:]...)
			break
		}
	}

	for _, v := range evt.Guilds {
		waitingGuilds[v.ID] = true
	}
	waitingGuildsMU.Unlock()

	RefreshStatus(ContextSession(data.Context()))

	// We pass the common.Session to the command system and that needs the user from the state
	common.BotSession.State.Lock()
	ready := discordgo.Ready{
		Version:   evt.Version,
		SessionID: evt.SessionID,
		User:      evt.User,
	}
	common.BotSession.State.Ready = ready
	common.BotSession.State.Unlock()

	var listedServers []int64
	err := common.RedisPool.Do(radix.Cmd(&listedServers, "SMEMBERS", "connected_guilds"))
	if err != nil {
		log.WithError(err).Error("Failed retrieving connected servers")
	}

	numShards := ShardManager.GetNumShards()

OUTER:
	for _, v := range listedServers {
		shard := (v >> 22) % int64(numShards)
		if int(shard) != data.Session.ShardID {
			continue
		}

		for _, readyGuild := range evt.Guilds {
			if readyGuild.ID == v {
				continue OUTER
			}
		}

		log.Info("Left server while bot was down: ", v)
		common.RedisPool.Do(radix.Cmd(nil, "SREM", "connected_guilds", discordgo.StrID(v)))
		go EmitGuildRemoved(v)

		if common.Statsd != nil {
			common.Statsd.Incr("yagpdb.left_guilds", nil, 1)
		}
	}
}

func HandleGuildCreate(evt *eventsystem.EventData) {
	g := evt.GuildCreate()
	log.WithFields(log.Fields{
		"g_name": g.Name,
		"guild":  g.ID,
	}).Debug("Joined guild")

	var n int
	err := common.RedisPool.Do(radix.Cmd(&n, "SADD", "connected_guilds", discordgo.StrID(g.ID)))
	if err != nil {
		log.WithError(err).Error("Redis error")
	}

	if n > 0 {
		log.WithField("g_name", g.Name).WithField("guild", g.ID).Info("Joined new guild!")
		go eventsystem.EmitEvent(&eventsystem.EventData{
			EvtInterface: g,
			Type:         eventsystem.EventNewGuild,
		}, eventsystem.EventNewGuild)

		if common.Statsd != nil {
			common.Statsd.Incr("yagpdb.joined_guilds", nil, 1)
		}
	}

	var banned bool
	common.RedisPool.Do(radix.Cmd(&banned, "SISMEMBER", "banned_servers", discordgo.StrID(g.ID)))
	if banned {
		log.WithField("guild", g.ID).Info("Banned server tried to add bot back")
		common.BotSession.ChannelMessageSend(g.ID, "This server is banned from using this bot. Join the support server for more info.")
		common.BotSession.GuildLeave(g.ID)
	}
}

func HandleGuildDelete(evt *eventsystem.EventData) {
	if evt.GuildDelete().Unavailable {
		// Just a guild outage
		return
	}

	log.WithFields(log.Fields{
		"g_name": evt.GuildDelete().Name,
	}).Info("Left guild")

	err := common.RedisPool.Do(radix.Cmd(nil, "SREM", "connected_guilds", discordgo.StrID(evt.GuildDelete().ID)))
	if err != nil {
		log.WithError(err).Error("Redis error")
	}

	go EmitGuildRemoved(evt.GuildDelete().ID)

	if common.Statsd != nil {
		common.Statsd.Incr("yagpdb.left_guilds", nil, 1)
	}
}

// StateHandler updates the world state
// use AddHandlerBefore to add handler before this one, otherwise they will alwyas be after
func StateHandler(evt *eventsystem.EventData) {
	State.HandleEvent(ContextSession(evt.Context()), evt.EvtInterface)
}

func HandleGuildUpdate(evt *eventsystem.EventData) {
	InvalidateCache(evt.GuildUpdate().Guild.ID, 0)
}

func HandleGuildRoleUpdate(evt *eventsystem.EventData) {
	InvalidateCache(evt.GuildRoleUpdate().GuildID, 0)
}

func HandleGuildRoleCreate(evt *eventsystem.EventData) {
	InvalidateCache(evt.GuildRoleCreate().GuildID, 0)
}

func HandleGuildRoleRemove(evt *eventsystem.EventData) {
	InvalidateCache(evt.GuildRoleDelete().GuildID, 0)
}

func HandleChannelCreate(evt *eventsystem.EventData) {
	InvalidateCache(evt.ChannelCreate().GuildID, 0)
}
func HandleChannelUpdate(evt *eventsystem.EventData) {
	InvalidateCache(evt.ChannelUpdate().GuildID, 0)
}
func HandleChannelDelete(evt *eventsystem.EventData) {
	InvalidateCache(evt.ChannelDelete().GuildID, 0)
}

func HandleGuildMemberUpdate(evt *eventsystem.EventData) {
	InvalidateCache(0, evt.GuildMemberUpdate().User.ID)
}

func InvalidateCache(guildID, userID int64) {
	if userID != 0 {
		common.RedisPool.Do(radix.Cmd(nil, "DEL", common.CacheKeyPrefix+discordgo.StrID(userID)+":guilds"))
	}
	if guildID != 0 {
		common.RedisPool.Do(radix.Cmd(nil, "DEL", common.CacheKeyPrefix+common.KeyGuild(guildID)))
		common.RedisPool.Do(radix.Cmd(nil, "DEL", common.CacheKeyPrefix+common.KeyGuildChannels(guildID)))
	}
}

func ConcurrentEventHandler(inner eventsystem.Handler) eventsystem.Handler {
	return func(evt *eventsystem.EventData) {
		go inner(evt)
	}
}

func HandleReactionAdd(evt *eventsystem.EventData) {
	ra := evt.MessageReactionAdd()
	if ra.GuildID != 0 {
		return
	}
	if ra.UserID == common.BotUser.ID {
		return
	}

	err := pubsub.Publish("dm_reaction", -1, ra)
	if err != nil {
		log.WithError(err).Error("failed publishing dm reaction")
	}
}

func HandleMessageCreate(evt *eventsystem.EventData) {
	mc := evt.MessageCreate()
	if mc.GuildID != 0 {
		return
	}

	if mc.Author == nil || mc.Author.ID == common.BotUser.ID {
		return
	}

	err := pubsub.Publish("dm_message", -1, mc)
	if err != nil {
		log.WithError(err).Error("failed publishing dm message")
	}
}
