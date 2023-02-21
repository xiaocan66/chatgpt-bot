package bot

import (
	"chatgpt-bot/cfg"
	"chatgpt-bot/constant"
	"chatgpt-bot/engine"
	"chatgpt-bot/model"
	"chatgpt-bot/utils"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type TelegramBot struct {
	tgBot    *tgbotapi.BotAPI
	engine   engine.Engine
	session  *sync.Map
	taskChan chan *model.ChatTask

	groupName   string
	channelName string
}

func (t *TelegramBot) Init(cfg *cfg.Config) error {
	if utils.IsAnyStringEmpty(cfg.BotConfig.TelegramBotToken,
		cfg.BotConfig.TelegramChannelName,
		cfg.BotConfig.TelegramGroupName) {
		return errors.New(constant.MISSING_REQUIRED_CONFIG)
	}
	t.channelName = cfg.BotConfig.TelegramChannelName
	t.groupName = cfg.BotConfig.TelegramGroupName

	t.session = &sync.Map{}
	bot, err := tgbotapi.NewBotAPI(cfg.BotConfig.TelegramBotToken)
	if err != nil {
		return err
	}
	t.tgBot = bot
	t.engine = engine.GetEngine(cfg.EngineConfig.EngineType)
	err = t.engine.Init(cfg)
	if err != nil {
		return err
	}

	t.taskChan = make(chan *model.ChatTask, 1)
	go t.loopAndFinishChatTask()
	log.Printf("[Init] telegram bot init success, bot name: %s", t.tgBot.Self.UserName)
	return nil
}

func NewTelegramBot() *TelegramBot {
	return &TelegramBot{}
}

func (t *TelegramBot) Run() {
	log.Println("[Run] start telegram bot")
	go t.fetchUpdates()
}

func (t *TelegramBot) fetchUpdates() {
	config := tgbotapi.NewUpdate(0)
	config.Timeout = 60

	botChannel := t.tgBot.GetUpdatesChan(config)
	for {
		select {
		case update, ok := <-botChannel:
			if !ok {
				botChannel = t.tgBot.GetUpdatesChan(config)
				log.Println("[FetchUpdates] channel closed, fetch again")
				continue
			}
			go t.handleUpdate(update)
		case <-time.After(30 * time.Second):
		}
	}
}

func (t *TelegramBot) loopAndFinishChatTask() {
	for {
		select {
		case task := <-t.taskChan:
			log.Println("[LoopAndFinishChatTask] got a task to finish")
			t.Finish(task)
		case <-time.After(30 * time.Second):
		}

	}
}

func (bot *TelegramBot) Finish(t *model.ChatTask) {
	log.Printf("[Finish] start chat task with question %s, chat id: %d, from: %d", t.Question, t.Chat, t.From)
	defer bot.session.Delete(t.From)

	res, err := bot.engine.Chat(t.Question)
	if err != nil {
		log.Printf("[Finish] chat task failed, err: %s", err)
		t.Answer = err.Error()
	} else {
		t.Answer = res
	}
	bot.Send(t)
	log.Printf("[Finish] end chat task with question %s, chat id: %d, from: %d", t.Question, t.Chat, t.From)

}

func (bot *TelegramBot) Send(t *model.ChatTask) {
	msg := tgbotapi.NewMessage(t.Chat, t.Question)
	msg.ParseMode = "markdown"
	msg.Text = t.Answer
	msg.ReplyToMessageID = t.MessageID
	bot.tgBot.Send(msg)
}

func (t *TelegramBot) handleUpdate(update tgbotapi.Update) {
	if update.Message == nil {
		return
	}
	log.Printf("[Update] 【chat】:%s, 【from】:%s, 【msg】:%s", utils.ToJsonString(update.Message.Chat),
		utils.ToJsonString(update.Message.From),
		utils.ToJsonString(update.Message))
	if update.Message.IsCommand() {
		msg := handleCommandMsg(update)
		t.tgBot.Send(msg)
	} else {
		t.handleUserMessage(update)
	}

}

func handleCommandMsg(update tgbotapi.Update) tgbotapi.MessageConfig {
	msg := tgbotapi.NewMessage(update.Message.Chat.ID, "")
	switch update.Message.Command() {
	case constant.START:
	case constant.CHATGPT:
		msg.Text = "Hi, I'm ChatGPT bot. I can chat with you. Just send me a sentence and I will reply you. \n\n 请在这条消息下回复你的问题，我会回复你的。"
	case constant.PING:
		msg.Text = "pong"
	default:
		msg.Text = "I don't know that command"
	}
	return msg
}

func shouldHandleMessage(update tgbotapi.Update, selfID int64) bool {
	isPrivate := update.Message.Chat.IsPrivate()
	shouldHandleMessage := isPrivate ||
		(update.Message.ReplyToMessage != nil &&
			update.Message.ReplyToMessage.From.ID == selfID)
	return shouldHandleMessage
}

func (t *TelegramBot) handleUserMessage(update tgbotapi.Update) {
	log.Printf("[HandleMessage] [%s] update id[%d], from id[%d], from name[%s], msg[%s], chat id[%d], chat name[%s]",
		update.Message.Chat.Type, update.UpdateID,
		update.Message.From.ID, fmt.Sprintf("%s %s %s", update.Message.From.FirstName, update.Message.From.LastName, update.Message.From.UserName),
		update.Message.Text, update.Message.Chat.ID, update.Message.Chat.Title)

	_, thisUserHasMessage := t.session.Load(update.Message.From.ID)

	if shouldIgnoreMsg(update) {
		return
	}

	if shouldHandleMessage(update, t.tgBot.Self.ID) {
		if t.shouldLimitUser(update) {
			t.sendLimitMessage(update.Message.Chat.ID, update.Message.MessageID)
			return
		}
		if !thisUserHasMessage {
			t.sendTaskToChannel(update.Message.Text, update.Message.Chat.ID, update.Message.From.ID, update.Message.MessageID)
		} else {
			log.Printf("[RateLimit] user %d is chatting with me, ignore message %s", update.Message.From.ID, update.Message.Text)
			t.sendRateLimitMessage(update.Message.Chat.ID)
		}
	}

}

func (t *TelegramBot) sendLimitMessage(chatID int64, msgID int) {
	text := fmt.Sprintf("You should join channel %s and group %s, then you can talk to me", t.channelName, t.groupName) +
		"\n\n" + fmt.Sprintf("你需要加入频道 %s 和群组 %s，然后才能和我交谈", t.channelName, t.groupName)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyToMessageID = msgID
	t.tgBot.Send(msg)
}

func (t *TelegramBot) findMemberFromChat(chatName string, userID int64) bool {
	findUserConfig := tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			SuperGroupUsername: chatName,
			UserID:             userID,
		},
	}
	member, err := t.tgBot.GetChatMember(findUserConfig)
	if err != nil || member.Status == "left" || member.Status == "kicked" {
		log.Printf("[ShouldLimitUser] memeber should be limit. id: %d", userID)
		return false
	}
	return true
}

func (t *TelegramBot) shouldLimitUser(update tgbotapi.Update) bool {
	userID := update.Message.From.ID
	canFindInChannel := t.findMemberFromChat(t.channelName, userID)
	canFindInGroup := t.findMemberFromChat(t.groupName, userID)

	return !(canFindInChannel && canFindInGroup)
}

func shouldIgnoreMsg(update tgbotapi.Update) bool {
	if update.Message == nil {
		return true
	}

	if update.Message.NewChatMembers != nil ||
		update.Message.LeftChatMember != nil {
		return true
	}

	if strings.Trim(update.Message.Text, " ") == "" {
		return true
	}

	return update.Message.ReplyToMessage != nil &&
		!update.Message.ReplyToMessage.From.IsBot
}

func (t *TelegramBot) sendRateLimitMessage(chat int64) {
	t.tgBot.Send(tgbotapi.NewMessage(chat, "you are chatting with me, please wait for a while."))
}

func (t *TelegramBot) sendTaskToChannel(question string, chat, from int64, msgID int) {
	t.session.Store(from, &struct{}{})
	log.Printf("[SendTaskToChannel] with question %s, chat id: %d, from: %d", question, chat, from)
	chatTask := model.NewChatTask(question, chat, from, msgID)
	t.taskChan <- chatTask
	t.sendTyping(chatTask)
}

func (t *TelegramBot) sendTyping(task *model.ChatTask) {
	t.tgBot.Send(tgbotapi.NewChatAction(task.Chat, tgbotapi.ChatTyping))
}