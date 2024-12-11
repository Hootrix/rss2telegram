package storage

//利用布隆过滤器实现推送状态存储
//每个rss地址对应一个bloom存储桶文件

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
)

const (
	// 布隆过滤器参数
	expectedItems = 100000 // 预期元素数量（10万）
	falsePositive = 0.001  // 误判率 0.1%
	// 状态过期时间
	stateExpirationDuration = 30 * 24 * time.Hour // 30 天

	//后缀
	bloomFileSuffix = ".bloom"
)

type ChannelState struct {
	filter    *bloom.BloomFilter
	updatedAt time.Time
}

type Storage struct {
	sync.RWMutex
	states  map[string]map[string]*ChannelState // feedURL -> channel -> state
	dataDir string
}

// rss发布状态的存储桶
func NewStorage(dataDir string) (*Storage, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	s := &Storage{
		states:  make(map[string]map[string]*ChannelState),
		dataDir: dataDir,
	}

	// 加载所有 channel 的状态
	files, err := filepath.Glob(filepath.Join(dataDir, "*"+bloomFileSuffix))
	if err != nil {
		return nil, err
	}

	// 遍历所有bloom文件
	for _, file := range files {
		// 从文件名中提取信息
		filename := filepath.Base(file)
		// 移除 .bloom 后缀
		encoded := strings.TrimSuffix(filename, bloomFileSuffix)
		decoded, err := base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decoding filename %s: %w", filename, err)
		}

		// 解析出 channel 和 URL
		parts := strings.SplitN(string(decoded), "|", 2)
		if len(parts) != 2 {
			log.Printf("Warning: invalid format file found: %s, will be recreated", file)
			continue
		}
		channel, feedURL := parts[0], parts[1]

		// 加载channel状态
		if err := s.loadChannelState(feedURL, channel); err != nil {
			return nil, fmt.Errorf("loading state for %s channel %s: %w", feedURL, channel, err)
		}
	}

	return s, nil
}

// 生成布隆过滤器的文件名
func (s *Storage) GenerateBloomFileName(feedURL string, channel string) string {
	// 使用channel和feedURL生成文件名
	data := channel + "|" + feedURL
	return base64.URLEncoding.EncodeToString([]byte(data))
}

// GetBloomFilePath 获取bloom过滤器的文件路径
func (s *Storage) GetBloomFilePath(feedURL string, channel string) string {
	return filepath.Join(s.dataDir, s.GenerateBloomFileName(feedURL, channel)+bloomFileSuffix)
}

// 检查item是否已经被处理
func (s *Storage) IsItemSeen(feedURL, feedName, channel, itemID string) bool {
	s.RLock()
	defer s.RUnlock()

	channelStates, exists := s.states[feedURL]
	if !exists {
		return false
	}

	state, exists := channelStates[channel]
	if !exists {
		return false
	}

	//如果返回 false，则元素一定不在集合中
	//如果返回 true，则元素可能在集合中（有一个很小的误判率）
	return state.filter.Test([]byte(itemID))
}

// 标记item为已处理
func (s *Storage) MarkItemSeen(feedURL, feedName, channel, itemID string) error {
	s.Lock()
	defer s.Unlock()

	// 确保feedURL的map存在
	channelStates, exists := s.states[feedURL]
	if !exists {
		channelStates = make(map[string]*ChannelState)
		s.states[feedURL] = channelStates
	}

	// 确保channel的state存在
	state, exists := channelStates[channel]
	if !exists {
		state = &ChannelState{
			filter:    bloom.NewWithEstimates(expectedItems, falsePositive),
			updatedAt: time.Now(),
		}
		channelStates[channel] = state
	}

	state.filter.Add([]byte(itemID))
	state.updatedAt = time.Now()

	// 保存状态到文件
	if err := s.saveChannelState(feedURL, channel, state); err != nil {
		return fmt.Errorf("error saving channel state: %w", err)
	}

	return nil
}

// 读取channel的持久化存储
func (s *Storage) loadChannelState(feedURL string, channel string) error {
	filepath := s.GetBloomFilePath(feedURL, channel)

	// 获取文件信息
	fileInfo, err := os.Stat(filepath)
	if err != nil {
		if os.IsNotExist(err) {
			// 如果文件不存在，创建新的状态
			s.states[feedURL] = make(map[string]*ChannelState)
			s.states[feedURL][channel] = &ChannelState{
				filter:    bloom.NewWithEstimates(expectedItems, falsePositive),
				updatedAt: time.Now(),
			}
			return nil
		}
		return fmt.Errorf("error getting file info: %w", err)
	}

	// 检查文件大小是否至少包含时间戳
	if fileInfo.Size() < 8 {
		return fmt.Errorf("invalid file size: %d bytes", fileInfo.Size())
	}

	// 打开文件
	file, err := os.OpenFile(filepath, os.O_RDONLY, 0644)
	if err != nil {
		return fmt.Errorf("error opening file: %w", err)
	}
	defer file.Close()

	// 读取时间戳(8字节)
	timeBytes := make([]byte, 8)
	if _, err := io.ReadFull(file, timeBytes); err != nil {
		return fmt.Errorf("error reading timestamp: %w", err)
	}
	timestamp := time.Unix(0, int64(binary.LittleEndian.Uint64(timeBytes)))

	// 检查是否过期(超过指定时间未更新rss)
	if time.Since(timestamp) > stateExpirationDuration {
		// 如果过期，创建新的状态
		s.states[feedURL] = make(map[string]*ChannelState)
		s.states[feedURL][channel] = &ChannelState{
			filter:    bloom.NewWithEstimates(expectedItems, falsePositive),
			updatedAt: time.Now(),
		}
		return nil
	}

	// 计算布隆过滤器数据的大小
	filterSize := fileInfo.Size() - 8 // 总大小减去时间戳大小
	if filterSize <= 0 {
		return fmt.Errorf("no filter data in file")
	}

	// 读取布隆过滤器数据
	filterData := make([]byte, filterSize)
	if _, err := io.ReadFull(file, filterData); err != nil {
		return fmt.Errorf("error reading filter data: %w", err)
	}

	// 创建新的布隆过滤器并反序列化数据
	filter := bloom.NewWithEstimates(expectedItems, falsePositive)
	if err := filter.UnmarshalBinary(filterData); err != nil {
		return fmt.Errorf("error unmarshaling filter: %w", err)
	}

	// 确保feedURL的map存在
	if _, exists := s.states[feedURL]; !exists {
		s.states[feedURL] = make(map[string]*ChannelState)
	}

	// 保存状态
	s.states[feedURL][channel] = &ChannelState{
		filter:    filter,
		updatedAt: timestamp,
	}

	return nil
}

// 将channel状态保存到文件
func (s *Storage) saveChannelState(feedURL string, channel string, state *ChannelState) error {
	filepath := s.GetBloomFilePath(feedURL, channel)

	// 创建临时文件
	tempFile := filepath + ".tmp"
	file, err := os.OpenFile(tempFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("error creating temp file: %w", err)
	}
	defer file.Close()

	// 写入时间戳
	timeBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(timeBytes, uint64(time.Now().UnixNano()))
	if _, err := file.Write(timeBytes); err != nil {
		return fmt.Errorf("error writing timestamp: %w", err)
	}

	// 获取并写入布隆过滤器数据
	filterData, err := state.filter.MarshalBinary()
	if err != nil {
		return fmt.Errorf("error marshaling filter: %w", err)
	}
	if _, err := file.Write(filterData); err != nil {
		return fmt.Errorf("error writing filter data: %w", err)
	}

	// 确保所有数据都写入磁盘
	if err := file.Sync(); err != nil {
		return fmt.Errorf("error syncing file: %w", err)
	}

	// 关闭文件
	if err := file.Close(); err != nil {
		return fmt.Errorf("error closing file: %w", err)
	}

	// 原子重命名
	if err := os.Rename(tempFile, filepath); err != nil {
		return fmt.Errorf("error renaming temp file: %w", err)
	}

	return nil
}

func (s *Storage) GetLastUpdated(feedURL string, channel string) time.Time {
	s.RLock()
	defer s.RUnlock()

	if channelStates, exists := s.states[feedURL]; exists {
		if state, exists := channelStates[channel]; exists {
			return state.updatedAt
		}
	}
	return time.Time{}
}
