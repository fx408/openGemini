// Copyright 2025 Huawei Cloud Computing Technologies Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type CollectSupported interface {
	Enabled() *bool
}

type Item interface {
	SetName(string)
	GetName() string
	GetValue() any
}

type BaseCollector struct {
	enabled bool
}

func (bc *BaseCollector) Enabled() *bool {
	return &bc.enabled
}

type CollectorProxy struct {
	mst     string
	tags    map[string]string
	items   []Item
	enabled *bool
}

func (c *CollectorProxy) Enabled() bool {
	return *c.enabled
}

func (c *CollectorProxy) SetMst(mst string) {
	mst = strings.ToLower(mst[:1]) + mst[1:]
	c.mst = mst
}

func (c *CollectorProxy) GetMst() string {
	return c.mst
}

func (c *CollectorProxy) SetTags(tags map[string]string) {
	c.tags = make(map[string]string)
	for k, v := range tags {
		c.tags[k] = v
	}
}

func (c *CollectorProxy) GetTags() map[string]string {
	return c.tags
}

func (c *CollectorProxy) AddItem(i Item) {
	c.items = append(c.items, i)
}

func (c *CollectorProxy) GetItems() []Item {
	return c.items
}

type ItemInt64 struct {
	atomic.Int64
	name string
}

func (i *ItemInt64) Incr() {
	i.Add(1)
}

func (i *ItemInt64) Decr() {
	i.Add(-1)
}

func (i *ItemInt64) SetName(name string) {
	i.name = name
}

func (i *ItemInt64) GetName() string {
	return i.name
}

func (i *ItemInt64) GetValue() any {
	return i.Load()
}

var collector = &Collector{values: make(map[string]interface{})}

func NewCollector() *Collector {
	return collector
}

type Collector struct {
	mu      sync.Mutex
	proxies []*CollectorProxy
	values  map[string]interface{}
}

func (c *Collector) SetGlobalTags(tags map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, proxy := range c.proxies {
		proxy.SetTags(tags)
	}
}

// Register objects that can be collected
// It should be called during program initialization, such as the init function
func (c *Collector) Register(obj CollectSupported) {
	proxy := &CollectorProxy{
		enabled: obj.Enabled(),
	}
	c.initStatObject(obj, proxy)
	c.proxies = append(c.proxies, proxy)
}

func (c *Collector) Collect(dst []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, proxy := range c.proxies {
		if proxy.Enabled() {
			dst = c.collect(dst, proxy)
		}
	}
	return dst, nil
}

func (c *Collector) collect(dst []byte, proxy *CollectorProxy) []byte {
	values := c.values
	clear(values)

	for _, item := range proxy.GetItems() {
		values[item.GetName()] = item.GetValue()
	}

	dst = AddPointToBuffer(proxy.GetMst(), proxy.GetTags(), values, dst)
	return dst
}

func (c *Collector) initStatObject(obj interface{}, proxy *CollectorProxy) {
	v := reflect.ValueOf(obj)
	if v.Kind() != reflect.Ptr {
		return
	}

	v = v.Elem()
	proxy.SetMst(v.Type().Name())

	for i := range v.NumField() {
		field := v.Field(i)

		// can customize the metric name by tag `name:"other_metric_name"`,
		// default value is the attribute name
		fieldName := v.Type().Field(i).Tag.Get("name")
		if fieldName == "" {
			fieldName = v.Type().Field(i).Name
		}

		if field.Kind() != reflect.Ptr {
			continue
		}

		// init attribute
		iv := reflect.New(field.Type().Elem())
		item, ok := iv.Interface().(Item)
		if !ok {
			continue
		}

		item.SetName(fieldName)
		proxy.AddItem(item)
		field.Set(iv)
	}
}

func MicroTimeUse(count, sum *ItemInt64) func() {
	count.Incr()
	begin := time.Now()
	return func() {
		sum.Add(time.Since(begin).Microseconds())
	}
}

func MilliTimeUse(count, sum *ItemInt64) func() {
	count.Incr()
	begin := time.Now()
	return func() {
		sum.Add(time.Since(begin).Milliseconds())
	}
}
