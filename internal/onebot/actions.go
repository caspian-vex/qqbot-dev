package onebot

import (
	"context"
	"fmt"
)

// LoginInfo 是 get_login_info 的返回。
type LoginInfo struct {
	UserID   int64  `json:"user_id"`
	Nickname string `json:"nickname"`
}

// GroupInfo 是 get_group_list 的元素。
type GroupInfo struct {
	GroupID        int64  `json:"group_id"`
	GroupName      string `json:"group_name"`
	MemberCount    int    `json:"member_count"`
	MaxMemberCount int    `json:"max_member_count"`
}

// FriendInfo 是 get_friend_list 的元素。
type FriendInfo struct {
	UserID   int64  `json:"user_id"`
	Nickname string `json:"nickname"`
	Remark   string `json:"remark"`
}

// SendResult 是发送消息类动作的返回。
type SendResult struct {
	MessageID int64 `json:"message_id"`
}

// GetLoginInfo 查询当前登录账号。
func (c *Client) GetLoginInfo(ctx context.Context) (*LoginInfo, error) {
	var out LoginInfo
	if err := c.Call(ctx, "get_login_info", nil, &out); err != nil {
		return nil, err
	}
	c.setStatus(func(s *Status) {
		s.SelfID = out.UserID
		s.Nickname = out.Nickname
	})
	return &out, nil
}

// GetGroupList 拉取群列表。
func (c *Client) GetGroupList(ctx context.Context) ([]GroupInfo, error) {
	var out []GroupInfo
	if err := c.Call(ctx, "get_group_list", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetFriendList 拉取好友列表。
func (c *Client) GetFriendList(ctx context.Context) ([]FriendInfo, error) {
	var out []FriendInfo
	if err := c.Call(ctx, "get_friend_list", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetOnline 查询协议端在线状态。
func (c *Client) GetOnline(ctx context.Context) (bool, error) {
	var out struct {
		Online bool `json:"online"`
		Good   bool `json:"good"`
	}
	if err := c.Call(ctx, "get_status", nil, &out); err != nil {
		return false, err
	}
	return out.Online && out.Good, nil
}

// SendGroupMsg 发送群消息。
func (c *Client) SendGroupMsg(ctx context.Context, groupID int64, segs []Segment) (int64, error) {
	var out SendResult
	params := map[string]any{
		"group_id":    groupID,
		"message":     segs,
		"auto_escape": false,
	}
	if err := c.Call(ctx, "send_group_msg", params, &out); err != nil {
		return 0, err
	}
	return out.MessageID, nil
}

// SendPrivateMsg 发送私聊消息。
func (c *Client) SendPrivateMsg(ctx context.Context, userID int64, segs []Segment) (int64, error) {
	var out SendResult
	params := map[string]any{
		"user_id":     userID,
		"message":     segs,
		"auto_escape": false,
	}
	if err := c.Call(ctx, "send_private_msg", params, &out); err != nil {
		return 0, err
	}
	return out.MessageID, nil
}

// Text 构造一个文本消息段。
func Text(s string) Segment {
	return Segment{Type: "text", Data: map[string]any{"text": s}}
}

// At 构造一个 @ 消息段。
func At(qq int64) Segment {
	return Segment{Type: "at", Data: map[string]any{"qq": fmt.Sprintf("%d", qq)}}
}

// Reply 构造一个引用回复消息段。
func Reply(messageID int64) Segment {
	return Segment{Type: "reply", Data: map[string]any{"id": fmt.Sprintf("%d", messageID)}}
}

// Image 构造一个图片消息段。
func Image(file string) Segment {
	return Segment{Type: "image", Data: map[string]any{"file": file}}
}

// TextSegments 把纯文本包装成单个文本段。
func TextSegments(s string) []Segment {
	return []Segment{Text(s)}
}
