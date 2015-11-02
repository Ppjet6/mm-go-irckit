package irckit

import (
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mattermost/platform/model"
	"github.com/sorcix/irc"
)

func NewUserMM(c net.Conn, srv Server) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})
	u.Srv = srv

	// used for login
	mattermostService := &User{Nick: "mattermost", User: "mattermost", Real: "ghost", Host: "abchost", channels: map[Channel]struct{}{}}
	mattermostService.MmGhostUser = true
	srv.Add(mattermostService)
	go srv.Handle(mattermostService)
	return u
}

func (u *User) loginToMattermost(url string, team string, email string, pass string) error {
	// login to mattermost
	MmClient := model.NewClient("https://" + url)
	myinfo, err := MmClient.LoginByEmail(team, email, pass)
	if err != nil {
		return err
	}
	u.MmUser = myinfo.Data.(*model.User)

	// setup websocket connection
	wsurl := "wss://" + url + "/api/v1/websocket"
	header := http.Header{}
	header.Set(model.HEADER_AUTH, "BEARER "+MmClient.AuthToken)
	WsClient, _, _ := websocket.DefaultDialer.Dial(wsurl, header)

	u.MmClient = MmClient
	u.MmWsClient = WsClient
	go u.WsReceiver()

	// populating users
	mmusers, _ := u.MmClient.GetProfiles(u.MmUser.TeamId, "")
	u.MmUsers = mmusers.Data.(map[string]*model.User)

	// populating channels
	mmchannels, _ := MmClient.GetChannels("")
	u.MmChannels = mmchannels.Data.(*model.ChannelList)

	// fetch users and channels from mattermost
	u.addUsersToChannels()

	return nil
}

func (u *User) addUsersToChannels() {
	srv := u.Srv
	rate := time.Second / 2
	throttle := time.Tick(rate)

	for _, mmchannel := range u.MmChannels.Channels {

		// exclude direct messages
		if strings.Contains(mmchannel.Name, "__") {
			continue
		}
		<-throttle
		go func(mmchannel *model.Channel) {
			edata, _ := u.MmClient.GetChannelExtraInfo(mmchannel.Id, "")
			for _, d := range edata.Data.(*model.ChannelExtra).Members {
				cghost, ok := srv.HasUser(d.Username)
				if !ok {
					ghost := &User{Nick: d.Username, User: d.Id,
						Real: "ghost", Host: u.MmClient.Url, channels: map[Channel]struct{}{}}

					ghost.MmGhostUser = true
					logger.Info("adding", ghost.Nick, "to #", mmchannel.Name)
					srv.Add(ghost)
					go srv.Handle(ghost)
					ch := srv.Channel("#" + mmchannel.Name)
					ch.Join(ghost)
				} else {
					ch := srv.Channel("#" + mmchannel.Name)
					ch.Join(cghost)
				}
			}
		}(mmchannel)
	}

	// add all users, also who are not on channels
	for _, mmuser := range u.MmUsers {
		_, ok := srv.HasUser(mmuser.Username)
		if !ok {
			ghost := &User{Nick: mmuser.Username, User: mmuser.Id,
				Real: "ghost", Host: u.MmClient.Url, channels: map[Channel]struct{}{}}
			ghost.MmGhostUser = true
			logger.Info("adding", ghost.Nick, "without a channel")
			srv.Add(ghost)
			go srv.Handle(ghost)
		}
	}
}

type MmInfo struct {
	MmGhostUser bool
	MmClient    *model.Client
	MmWsClient  *websocket.Conn
	Srv         Server
	MmUsers     map[string]*model.User
	MmUser      *model.User
	MmChannels  *model.ChannelList
}

func (u *User) WsReceiver() {
	var rmsg model.Message
	for {
		if err := u.MmWsClient.ReadJSON(&rmsg); err != nil {
			logger.Critical(err)
			os.Exit(1)
		}
		logger.Debugf("%#v", rmsg)
		if rmsg.Action == model.ACTION_POSTED {
			data := model.PostFromJson(strings.NewReader(rmsg.Props["post"]))
			logger.Debug("receiving userid", data.UserId)
			if data.UserId == u.MmUser.Id {
				// our own message
				continue
			}
			// we don't have the user, refresh the userlist
			if u.MmUsers[data.UserId] == nil {
				mmusers, _ := u.MmClient.GetProfiles(u.MmUser.TeamId, "")
				u.MmUsers = mmusers.Data.(map[string]*model.User)
			}
			ghost, _ := u.Srv.HasUser(u.MmUsers[data.UserId].Username)
			rcvchannel := u.getMMChannelName(data.ChannelId)
			if strings.Contains(rcvchannel, "__") {
				var rcvuser string
				rcvusers := strings.Split(rcvchannel, "__")
				if rcvusers[0] != u.MmUser.Id {
					rcvuser = u.MmUsers[rcvusers[0]].Username
				} else {
					rcvuser = u.MmUsers[rcvusers[1]].Username
				}

				u.Encode(&irc.Message{
					Prefix:   &irc.Prefix{Name: rcvuser, User: rcvuser, Host: rcvuser},
					Command:  irc.PRIVMSG,
					Params:   []string{u.Nick},
					Trailing: data.Message,
				})
				//u.Srv.Publish(&event{UserMsgEvent, u.Srv, nil, u, msg})
				continue
			}

			ch := u.Srv.Channel("#" + u.getMMChannelName(data.ChannelId))
			msgs := strings.Split(data.Message, "\n")
			for _, m := range msgs {
				ch.Message(ghost, m)
			}
			//ch := srv.Channel("#" + data.Channel)

			//mychan[0].Message(ghost, data.Message)
			logger.Debug(u.MmUsers[data.UserId].Username, ":", data.Message)
			logger.Debugf("%#v", data)
		}
	}
}

func (u *User) getMMChannelName(id string) string {
	for _, channel := range u.MmChannels.Channels {
		if channel.Id == id {
			return channel.Name
		}
	}
	return ""
}

func (u *User) getMMChannelId(name string) string {
	for _, channel := range u.MmChannels.Channels {
		if channel.Name == name {
			return channel.Id
		}
	}
	return ""
}

func (u *User) getMMUserId(name string) string {
	for id, u := range u.MmUsers {
		if u.Username == name {
			return id
		}
	}
	return ""
}

func (u *User) MsgUser(toUser *User, msg string) {
	u.Encode(&irc.Message{
		Prefix:   toUser.Prefix(),
		Command:  irc.PRIVMSG,
		Params:   []string{u.Nick},
		Trailing: msg,
	})
}

func (u *User) handleMMDM(toUser *User, msg string) {
	var channel string
	// We don't have a DM with this user yet.
	if u.getMMChannelId(toUser.User+"__"+u.MmUser.Id) == "" && u.getMMChannelId(u.MmUser.Id+"__"+toUser.User) == "" {
		// create DM channel
		_, err := u.MmClient.CreateDirectChannel(map[string]string{"user_id": toUser.User})
		if err != nil {
			logger.Debugf("direct message to %#v failed: %s", toUser, err)
		}
		// update our channels
		mmchannels, _ := u.MmClient.GetChannels("")
		u.MmChannels = mmchannels.Data.(*model.ChannelList)
	}

	// build the channel name
	if toUser.User > u.MmUser.Id {
		channel = u.MmUser.Id + "__" + toUser.User
	} else {
		channel = toUser.User + "__" + u.MmUser.Id
	}
	// build & send the message
	post := &model.Post{ChannelId: u.getMMChannelId(channel), Message: msg}
	u.MmClient.CreatePost(post)
}

func (u *User) handleMMServiceBot(toUser *User, msg string) {
	commands := strings.Fields(msg)
	switch commands[0] {
	case "LOGIN":
		{
			data := strings.Split(msg, " ")
			if len(data) != 5 {
				u.MsgUser(toUser, "need LOGIN <server> <team> <login> <pass>")
				return
			}
			err := u.loginToMattermost(data[1], data[2], data[3], data[4])
			if err != nil {
				u.MsgUser(toUser, "login failed")
				return
			}
			u.MsgUser(toUser, "login OK")
		}
	default:
		u.MsgUser(toUser, "need LOGIN <server> <team> <login> <pass>")
	}

}
