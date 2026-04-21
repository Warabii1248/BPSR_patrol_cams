package global

import (
	"sync"
)

// 场景怪物数据
var SceneMonsterList = make(map[uint64]*Monster)
var SceneMonsterListLock = sync.RWMutex{}

var CurrentScene *SceneInfo = nil
var CurrentSceneLock = sync.RWMutex{}

type SceneInfo struct {
	Scene  *SceneData   `json:"scene"`  //当前场景信息
	Player *ScenePlayer `json:"player"` //当前场景自己的信息
}
type SceneData struct {
	MapId  uint32 `json:"map_id"`  //场景ID
	Name   string `json:"name"`    //场景名称
	LineId uint32 `json:"line_id"` //场景线路ID
}
type ScenePlayer struct {
	Id         uint64    `json:"id"`            //自己ID
	FightPoint int32     `json:"fight_point"`   //评分
	Name       string    `json:"name"`          //自己昵称
	Level      int32     `json:"level"`         //玩家等级
	Hp         int64     `json:"hp"`            //当前血量
	MaxHp      int64     `json:"max_hp"`        //最大血量
	Pos        *Position `json:"pos,omitempty"` //自己坐标
}

type Position struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
	Z float32 `json:"z"`
}
type AttackPlayer struct {
	Name           string `json:"name,omitempty"` //玩家昵称
	LastAttackTime int64  `json:"-"`              //最后攻击时间
}
type Monster struct {
	Name          string                   `json:"name,omitempty"`           //怪物名称
	Hp            uint64                   `json:"hp"`                       //当前血量
	MaxHp         uint64                   `json:"max_hp,omitempty"`         //最大血量
	Pos           *Position                `json:"pos,omitempty"`            //怪物坐标
	TemplateId    uint64                   `json:"template_id,omitempty"`    //模板ID
	EntityId      uint64                   `json:"entity_id,omitempty"`      //当前敌人ID
	AttackPlayers map[uint64]*AttackPlayer `json:"attack_players,omitempty"` //正在攻击的玩家列表
	UpdateTime    int64                    `json:"-"`                        //数据最后更新时间
}

