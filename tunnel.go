package main

import (
	"encoding/base32"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

var decoder = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding('0')

type tunnel struct {
	Messages    chan string
	Cancel      chan struct{}
	fgLists     map[string]*fragmentList
	fgListsLock sync.Mutex
	topDomain   string
	domains     chan string
}

type fragmentList struct {
	totalSize int
	fragments map[int]fragment
	expiresAt time.Time
}

type fragment struct {
	id        string
	totalSize int
	offset    int
	data      string
}

func newTunnel(topDomain string, expiration time.Duration, deletionInterval time.Duration) *tunnel {
	tun := &tunnel{
		Messages:  make(chan string, 256),
		Cancel:    make(chan struct{}),
		topDomain: topDomain,
		domains:   make(chan string, 256),
		fgLists:   make(map[string]*fragmentList),
	}
	go tun.listenDomains(expiration)
	go tun.removeExpiredMessages(deletionInterval)
	return tun
}

func parseDomain(topDomain string, domain string) (fragment, error) {
	if !strings.HasSuffix(domain, "."+topDomain) {
		return fragment{}, fmt.Errorf("Domain %s does not have top domain %s", domain, topDomain)
	}
	payload := strings.TrimSuffix(domain, "."+topDomain)
	labels := strings.Split(payload, ".")
	if len(labels) < 4 {
		return fragment{}, fmt.Errorf("Domain has %d labels but expected at least 4", len(labels))
	}
	id := labels[0]

	totalSize, err := strconv.Atoi(labels[1])
	if err != nil {
		return fragment{}, err
	}

	offset, err := strconv.Atoi(labels[2])
	if err != nil {
		return fragment{}, err
	}

	data := strings.Join(labels[3:], "")

	return fragment{
		id:        id,
		totalSize: totalSize,
		offset:    offset,
		data:      data,
	}, nil
}

func (fl fragmentList) assemble() (string, error) {
	buf := make([]byte, fl.totalSize)
	for _, f := range fl.fragments {
		if f.offset >= fl.totalSize {
			return "", fmt.Errorf("Offset %d > total size %d", f.offset, fl.totalSize)
		}
		copy(buf[f.offset:], []byte(f.data))
	}
	dec, err := decoder.DecodeString(string(buf))
	if err != nil {
		return "", err
	}
	return string(dec), nil
}

func (tun *tunnel) listenDomains(expiration time.Duration) {
	for {
		select {
		case <-tun.Cancel:
			return
		case domain := <-tun.domains:
			func() {
				tun.fgListsLock.Lock()
				defer tun.fgListsLock.Unlock()

				fg, err := parseDomain(tun.topDomain, domain)
				if err != nil {
					log.Println(err)
					return
				}

				if _, ok := tun.fgLists[fg.id]; !ok {
					tun.fgLists[fg.id] = &fragmentList{
						totalSize: 0,
						fragments: make(map[int]fragment),
						expiresAt: time.Now().Add(expiration),
					}
				}
				fgList := tun.fgLists[fg.id]
				fgList.totalSize = fg.totalSize
				fgList.fragments[fg.offset] = fg
				fgList.expiresAt = time.Now().Add(expiration)

				totalBytes := 0
				for _, fg := range fgList.fragments {
					totalBytes += len(fg.data)
				}

				if totalBytes >= fgList.totalSize {
					msg, err := fgList.assemble()
					if err != nil {
						log.Println(err)
						return
					}
					tun.Messages <- msg
					delete(tun.fgLists, fg.id)
				}
			}()
		}
	}
}

func (tun *tunnel) removeExpiredMessages(deletionInterval time.Duration) {
	ticker := time.NewTicker(deletionInterval)
	for {
		select {
		case <-tun.Cancel:
			ticker.Stop()
			return
		case <-ticker.C:
			tun.fgListsLock.Lock()
			now := time.Now()
			for id, fgList := range tun.fgLists {
				if fgList.expiresAt.Before(now) {
					delete(tun.fgLists, id)
				}
			}
			tun.fgListsLock.Unlock()
		}
	}
}

func (tun *tunnel) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) < 1 {
		return
	}

	domain := r.Question[0].Name
	if r.Question[0].Qtype == dns.TypeA {
		tun.domains <- domain
	}

	m := &dns.Msg{}
	m.SetReply(r)
	m.Answer = []dns.RR{
		&dns.CNAME{
			Hdr:    dns.RR_Header{Name: domain, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 0},
			Target: "blackhole-1.iana.org.",
		},
	}
	err := w.WriteMsg(m)
	if err != nil {
		log.Println(err)
	}
}
