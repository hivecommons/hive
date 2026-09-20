const { Client, GatewayIntentBits, Events } = require('discord.js');
const { loadConfig } = require('./lib/config');
const { DashboardBridge } = require('./lib/dashboard-bridge');
const { PipelineWatcher } = require('./lib/pipeline-watcher');
const { CommandRouter } = require('./lib/command-router');
const { agentNames } = require('./lib/agent-identities');

const MSG_QUEUE_INTERVAL_MS = 1200;
const STATUS_HEARTBEAT_MS = 15 * 60 * 1000; // 15 minutes

const config = loadConfig();

if (!config.botToken) {
  console.error('DISCORD_BOT_TOKEN not set');
  process.exit(1);
}
if (!config.channelPrimary) {
  console.error('DISCORD_CHANNEL_PRIMARY not set');
  process.exit(1);
}

const client = new Client({
  intents: [
    GatewayIntentBits.Guilds,
    GatewayIntentBits.GuildMessages,
    GatewayIntentBits.MessageContent,
  ],
});

const router = new CommandRouter(config);

const messageQueue = [];
let draining = false;

function drainQueue() {
  if (draining) return;
  draining = true;
  const interval = setInterval(async () => {
    if (messageQueue.length === 0) {
      clearInterval(interval);
      draining = false;
      return;
    }
    const item = messageQueue.shift();
    try {
      const channelId = item.target === 'alerts' ? config.channelAlerts : config.channelPrimary;
      const channel = await client.channels.fetch(channelId);
      if (!channel) return;
      if (item.embed) {
        await channel.send({ embeds: [item.embed] });
      } else {
        await channel.send(item.text);
      }
    } catch (err) {
      console.error('Discord send error:', err.message);
    }
  }, MSG_QUEUE_INTERVAL_MS);
}

function sendMessage(text, target) {
  messageQueue.push({ text, target: target || 'primary' });
  drainQueue();
}

function sendEmbed(embed, target) {
  messageQueue.push({ embed, target: target || 'primary' });
  drainQueue();
}

async function setTopic(topic) {
  try {
    const channel = await client.channels.fetch(config.channelPrimary);
    if (channel && channel.setTopic) {
      await channel.setTopic(topic);
    }
  } catch (err) {
    console.error('Topic update error:', err.message);
  }
}

const bridge = new DashboardBridge(config, sendMessage, sendEmbed, setTopic);
const watcher = new PipelineWatcher(config, sendMessage, sendEmbed);

let statusInterval = null;

client.once(Events.ClientReady, (c) => {
  console.log(`Hive bot logged in as ${c.user.tag}`);
  bridge.start();
  watcher.start();
  sendMessage('⚙️ **[pipeline]** Hive Discord bot online');

  statusInterval = setInterval(async () => {
    try {
      const status = await router.handle('status', 'heartbeat');
      sendMessage(`📊 **Status heartbeat**\n${status}`);
    } catch (err) {
      console.error('Status heartbeat error:', err.message);
    }
  }, STATUS_HEARTBEAT_MS);
});

// Fail closed: commands are only honoured in the configured operator
// channel(s). Without this gate any guild member in ANY channel the bot can
// read could kick agents with arbitrary prompts or pause/resume the fleet —
// the README documents the private-channel boundary, this enforces it (#7822).
const commandChannels = new Set(
  [config.channelPrimary, config.channelAlerts].filter(Boolean)
);

client.on(Events.MessageCreate, async (message) => {
  if (message.author.bot) return;
  if (!commandChannels.has(message.channel.id)) return;

  let content = '';

  if (message.mentions.has(client.user)) {
    content = message.content.replace(/<@!?\d+>/g, '').trim();
  } else if (message.content.startsWith(config.commandPrefix)) {
    content = message.content.slice(config.commandPrefix.length).trim();
  } else {
    return;
  }

  if (!content) {
    await message.reply(await router.handle('help', message.author.tag));
    return;
  }

  const response = await router.handle(content, message.author.tag);
  await message.reply(response);
});

process.on('SIGTERM', () => {
  console.log('SIGTERM received, shutting down');
  clearInterval(statusInterval);
  bridge.stop();
  watcher.stop();
  client.destroy();
  process.exit(0);
});

process.on('SIGINT', () => {
  clearInterval(statusInterval);
  bridge.stop();
  watcher.stop();
  client.destroy();
  process.exit(0);
});

client.login(config.botToken);
