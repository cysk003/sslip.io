// Package xip provides functions to create a DNS server which, when queried
// with a hostname with an embedded IP address, returns that IP Address.  It
// was inspired by xip.io, which was created by Sam Stephenson
package xip

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/net/dns/dnsmessage"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

//counterfeiter:generate . V3client
type V3client interface {
	Get(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error)
	Put(context.Context, string, string, ...clientv3.OpOption) (*clientv3.PutResponse, error)
	Delete(context.Context, string, ...clientv3.OpOption) (*clientv3.DeleteResponse, error)
	Close() error
}

// Xip is meant to be a singleton that holds global state for the DNS server
type Xip struct {
	Etcd                        V3client      // etcd client for `k-v.io`
	DnsAmplificationAttackDelay chan struct{} // for throttling metrics.status.sslip.io
	Metrics                     Metrics       // DNS server metrics
	BlocklistStrings            []string      // list of blacklisted strings that shouldn't appear in public hostnames
	BlocklistCDIRs              []net.IPNet   // list of blacklisted strings that shouldn't appear in public hostnames
	BlocklistUpdated            time.Time     // The most recent time the Blocklist was updated
}

// Metrics contains the counters of the important/interesting queries
type Metrics struct {
	Start                           time.Time
	Queries                         int
	AnsweredQueries                 int
	AnsweredAQueries                int
	AnsweredAAAAQueries             int
	AnsweredTXTSrcIPQueries         int
	AnsweredXTVersionQueries        int
	AnsweredNSDNS01ChallengeQueries int
	AnsweredBlockedQueries          int
}

// DomainCustomization is a value that is returned for a specific query.
// The map key is the domain in question, e.g. "sslip.io." (always include trailing dot).
// For example, when querying for MX records for "sslip.io", return the protonmail servers,
// but when querying for MX records for generic queries, e.g. "127.0.0.1.sslip.io", return the
// default (which happens to be no MX records).
//
// Noticeably absent are the NS records and SOA records. They don't need to be customized
// because they are always the same, regardless of the domain being queried.
type DomainCustomization struct {
	A     []dnsmessage.AResource
	AAAA  []dnsmessage.AAAAResource
	CNAME dnsmessage.CNAMEResource
	MX    []dnsmessage.MXResource
	TXT   func(*Xip, net.IP) ([]dnsmessage.TXTResource, error)
	// Unlike the other record types, TXT is a function in order to enable more complex behavior
	// e.g. IP address of the query's source
}

// DomainCustomizations is a lookup table for specially-crafted records
// e.g. MX records for sslip.io.
// The string key should always be lower-cased
// DomainCustomizations{"sslip.io": ...} NOT DomainCustomizations{"sSLip.iO": ...}
// DNS hostnames are technically case-insensitive
type DomainCustomizations map[string]DomainCustomization

// KvCustomizations is a lookup table for custom TXT records
// e.g. KvCustomizations["my-key"] = []dnsmessage.TXTResource{ TXT: { "my-value" } }
// The key should NOT include ".k-v.io."
type KvCustomizations map[string][]dnsmessage.TXTResource

// There's nothing like global variables to make my heart pound with joy.
// Some of these are global because they are, in essence, constants which
// I don't want to waste time recreating with every function call.
var (
	ipv4REDots   = regexp.MustCompile(`(^|[.-])(((25[0-5]|(2[0-4]|1?[0-9])?[0-9])\.){3}(25[0-5]|(2[0-4]|1?[0-9])?[0-9]))($|[.-])`)
	ipv4REDashes = regexp.MustCompile(`(^|[.-])(((25[0-5]|(2[0-4]|1?[0-9])?[0-9])-){3}(25[0-5]|(2[0-4]|1?[0-9])?[0-9]))($|[.-])`)
	// https://stackoverflow.com/questions/53497/regular-expression-that-matches-valid-ipv6-addresses
	ipv6RE           = regexp.MustCompile(`(^|[.-])(([0-9a-fA-F]{1,4}-){7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}-){1,7}-|([0-9a-fA-F]{1,4}-){1,6}-[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}-){1,5}(-[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}-){1,4}(-[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}-){1,3}(-[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}-){1,2}(-[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}-((-[0-9a-fA-F]{1,4}){1,6})|-((-[0-9a-fA-F]{1,4}){1,7}|-)|fe80-(-[0-9a-fA-F]{0,4}){0,4}%[0-9a-zA-Z]+|--(ffff(-0{1,4})?-)?((25[0-5]|(2[0-4]|1?[0-9])?[0-9])\.){3}(25[0-5]|(2[0-4]|1?[0-9])?[0-9])|([0-9a-fA-F]{1,4}-){1,4}-((25[0-5]|(2[0-4]|1?[0-9])?[0-9])\.){3}(25[0-5]|(2[0-4]|1?[0-9])?[0-9]))($|[.-])`)
	dns01ChallengeRE = regexp.MustCompile(`(?i)_acme-challenge\.`)
	kvRE             = regexp.MustCompile(`\.k-v\.io\.$`)
	nsAwsSslip, _    = dnsmessage.NewName("ns-aws.sslip.io.")
	nsAzureSslip, _  = dnsmessage.NewName("ns-azure.sslip.io.")
	nsGceSslip, _    = dnsmessage.NewName("ns-gce.sslip.io.")
	NameServers      = []dnsmessage.NSResource{
		{NS: nsAwsSslip},
		{NS: nsAzureSslip},
		{NS: nsGceSslip},
	}

	mbox, _  = dnsmessage.NewName("briancunnie.gmail.com.")
	mx1, _   = dnsmessage.NewName("mail.protonmail.ch.")
	mx2, _   = dnsmessage.NewName("mailsec.protonmail.ch.")
	dkim1, _ = dnsmessage.NewName("protonmail.domainkey.dw4gykv5i2brtkjglrf34wf6kbxpa5hgtmg2xqopinhgxn5axo73a.domains.proton.ch.")
	dkim2, _ = dnsmessage.NewName("protonmail2.domainkey.dw4gykv5i2brtkjglrf34wf6kbxpa5hgtmg2xqopinhgxn5axo73a.domains.proton.ch.")
	dkim3, _ = dnsmessage.NewName("protonmail3.domainkey.dw4gykv5i2brtkjglrf34wf6kbxpa5hgtmg2xqopinhgxn5axo73a.domains.proton.ch.")

	VersionSemantic = "0.0.0"
	VersionDate     = "0001/01/01-99:99:99-0800"
	VersionGitHash  = "cafexxx"

	MetricsBufferSize = 100 // big enough to run our tests, and small enough to prevent DNS amplification attacks

	// etcdContextTimeout — the duration (context) that we wait for etcd to get back to us
	// - etcd queries on the nameserver take as long as 482 milliseconds on the "slow" server, 247 on the "fast"
	// - round-trip time from my house in San Francisco to ns-azure in Singapore is 190 milliseconds
	// - time between queries with `dig` on my macOS Monterey is 5000 milliseconds, three queries before giving up
	// - quadruple the headroom for queries 4 x 482 = 1928, should still leave enough room to get answer back
	//   within the 5000 milliseconds
	etcdContextTimeout = 1928 * time.Millisecond

	TxtKvCustomizations = KvCustomizations{}
	Customizations      = DomainCustomizations{
		"sslip.io.": {
			A: []dnsmessage.AResource{
				{A: [4]byte{78, 46, 204, 247}},
			},
			AAAA: []dnsmessage.AAAAResource{
				{AAAA: [16]byte{42, 1, 4, 248, 12, 23, 11, 143, 0, 0, 0, 0, 0, 0, 0, 2}},
			},
			MX: []dnsmessage.MXResource{
				{
					Pref: 10,
					MX:   mx1,
				},
				{
					Pref: 20,
					MX:   mx2,
				},
			},
			TXT: func(_ *Xip, _ net.IP) ([]dnsmessage.TXTResource, error) {
				// Although multiple TXT records with multiple strings are allowed, we're sticking
				// with a multiple TXT records with a single string apiece because that's what ProtonMail requires
				// and that's what google.com does.
				return []dnsmessage.TXTResource{
					{TXT: []string{"protonmail-verification=ce0ca3f5010aa7a2cf8bcc693778338ffde73e26"}}, // ProtonMail verification; don't delete
					{TXT: []string{"v=spf1 include:_spf.protonmail.ch mx ~all"}},
				}, nil // Sender Policy Framework
			},
		},
		// a global nameserver for sslip.io, a conglomeration of ns-{aws,azure,gce}.sslip.io
		"ns.sslip.io.": {
			A: []dnsmessage.AResource{
				{A: [4]byte{52, 0, 56, 137}},
				{A: [4]byte{52, 187, 42, 158}},
				{A: [4]byte{104, 155, 144, 4}},
			},
			AAAA: []dnsmessage.AAAAResource{{AAAA: [16]byte{0x26, 0, 0x1f, 0x18, 0x0a, 0xaf, 0x69, 0, 0, 0, 0, 0, 0, 0, 0, 0xa}}},
		},
		// nameserver addresses; we get queries for those every once in a while
		"ns-aws.sslip.io.": {
			A:    []dnsmessage.AResource{{A: [4]byte{52, 0, 56, 137}}},
			AAAA: []dnsmessage.AAAAResource{{AAAA: [16]byte{0x26, 0, 0x1f, 0x18, 0x0a, 0xaf, 0x69, 0, 0, 0, 0, 0, 0, 0, 0, 0xa}}},
		},
		"ns-azure.sslip.io.": {A: []dnsmessage.AResource{{A: [4]byte{52, 187, 42, 158}}}},
		"ns-gce.sslip.io.":   {A: []dnsmessage.AResource{{A: [4]byte{104, 155, 144, 4}}}},
		// CNAMEs for sslip.io for DKIM signing
		"protonmail._domainkey.sslip.io.": {
			CNAME: dnsmessage.CNAMEResource{
				CNAME: dkim1,
			},
		},
		"protonmail2._domainkey.sslip.io.": {
			CNAME: dnsmessage.CNAMEResource{
				CNAME: dkim2,
			},
		},
		"protonmail3._domainkey.sslip.io.": {
			CNAME: dnsmessage.CNAMEResource{
				CNAME: dkim3,
			},
		},
		// Special-purpose TXT records
		"ip.sslip.io.": {
			TXT: ipSslipIo,
		},
		"version.status.sslip.io.": {
			TXT: func(x *Xip, _ net.IP) ([]dnsmessage.TXTResource, error) {
				x.Metrics.AnsweredXTVersionQueries++
				return []dnsmessage.TXTResource{
					{TXT: []string{VersionSemantic}}, // e.g. "2.2.1'
					{TXT: []string{VersionDate}},     // e.g. "2021/10/03-15:08:54+0100"
					{TXT: []string{VersionGitHash}},  // e.g. "9339c0d"
				}, nil
			},
		},
		"metrics.status.sslip.io.": {
			TXT: metricsSslipIo,
		},
	}
)

// Response Why do I have a crazy struct of fields of arrays of functions?
// It's because I can't use dnsmessage.Builder as I had hoped; specifically
// I need to set the Header _after_ I process the message, but Builder expects
// it to be set first, so I use the functions as a sort of batch process to
// create the Builder. What in Header needs to be tweaked? Certain TXT records
// need to unset the authoritative field, and queries for ANY record need
// to set the rcode.
type Response struct {
	Header      dnsmessage.Header
	Answers     []func(*dnsmessage.Builder) error
	Authorities []func(*dnsmessage.Builder) error
	Additionals []func(*dnsmessage.Builder) error
}

// NewXip follows convention for constructors: https://go.dev/doc/effective_go#allocation_new
func NewXip(etcdEndpoint, blocklistURL string) (x *Xip, logmessages []string) {
	var err error
	x = &Xip{Metrics: Metrics{Start: time.Now()}}
	// connect to `etcd`; if there's an error, set etcdCli to `nil` and that to
	// determine whether to use a local key-value store instead
	x.Etcd, err = clientv3New(etcdEndpoint)
	if err != nil {
		logmessages = append(logmessages, fmt.Sprintf("failed to connect to etcd at %s; using local key-value store instead: %s", etcdEndpoint, err.Error()))
	} else {
		logmessages = append(logmessages, fmt.Sprintf("Successfully connected to etcd at %s", etcdEndpoint))
	}
	// don't `defer etcdCli.Close()`: "The Client has internal state (watchers and leases), so
	// Clients should be reused instead of created as needed"

	// re-download the blocklist every hour so I don't need to restart servers after updating blocklist
	go func() {
		for {
			blocklistStrings, blocklistCDIRs, err := readBlocklist(blocklistURL)
			if err != nil {
				logmessages = append(logmessages, fmt.Sprintf("couldn't get blocklist at %s: %s", blocklistURL, err.Error()))
			} else {
				logmessages = append(logmessages, fmt.Sprintf("Successfully downloaded blocklist from %s: %v, %v", blocklistURL, blocklistStrings, blocklistCDIRs))
				x.BlocklistStrings = blocklistStrings
				x.BlocklistCDIRs = blocklistCDIRs
				x.BlocklistUpdated = time.Now()
			}
			time.Sleep(1 * time.Hour)
		}
	}()

	// We want to make sure that our DNS server isn't used in a DNS amplification attack.
	// The endpoint we're worried about is metrics.status.sslip.io, whose reply is
	// ~400 bytes with a query of ~100 bytes (4x amplification). We accomplish this by
	// using channels with a quarter-second delay. Max throughput 1.2 kBytes/sec.
	//
	// We want to balance this delay against our desire to run tests quickly, so we buffer
	// the channel with enough room to accommodate our tests.
	//
	// We also want to have fun playing with channels
	dnsAmplificationAttackDelay := make(chan struct{}, MetricsBufferSize)
	x.DnsAmplificationAttackDelay = dnsAmplificationAttackDelay
	go func() {
		// fill up the channel's buffer so that our tests aren't slowed down (~85 tests)
		for i := 0; i < MetricsBufferSize; i++ {
			dnsAmplificationAttackDelay <- struct{}{}
		}
		// now put on the brakes for users trying to leverage our server in a DNS amplification attack
		for {
			dnsAmplificationAttackDelay <- struct{}{}
			time.Sleep(250 * time.Millisecond)
		}
	}()
	return x, logmessages
}

// QueryResponse takes in a raw (packed) DNS query and returns a raw (packed)
// DNS response, a string (for logging) that describes the query and the
// response, and an error. It takes in the raw data to offload as much as
// possible from main(). main() is hard to unit test, but functions like
// QueryResponse are not as hard.
//
// Examples of log strings returned:
//   78.46.204.247.33654: TypeA 127-0-0-1.sslip.io ? 127.0.0.1
//   78.46.204.247.33654: TypeA non-existent.sslip.io ? nil, SOA
//   78.46.204.247.33654: TypeNS www.example.com ? NS
//   78.46.204.247.33654: TypeSOA www.example.com ? SOA
//   2600::.33654: TypeAAAA --1.sslip.io ? ::1
func (x *Xip) QueryResponse(queryBytes []byte, srcAddr net.IP) (responseBytes []byte, logMessage string, err error) {
	var queryHeader dnsmessage.Header
	var p dnsmessage.Parser
	var response Response

	if queryHeader, err = p.Start(queryBytes); err != nil {
		return nil, "", err
	}
	var q dnsmessage.Question
	// we only answer the first question even though there technically may be more than one;
	// de facto there's one and only one question
	if q, err = p.Question(); err != nil {
		return nil, "", err
	}
	response, logMessage, err = x.processQuestion(q, srcAddr)
	if err != nil {
		return nil, "", err
	}
	response.Header.ID = queryHeader.ID
	response.Header.RecursionDesired = queryHeader.RecursionDesired
	x.Metrics.Queries++

	b := dnsmessage.NewBuilder(nil, response.Header)
	b.EnableCompression()
	if err = b.StartQuestions(); err != nil {
		return nil, "", err
	}
	if err = b.Question(q); err != nil {
		return
	}
	if err = b.StartAnswers(); err != nil {
		return nil, "", err
	}
	for _, answer := range response.Answers {
		if err = answer(&b); err != nil {
			return nil, "", err
		}
	}
	if err = b.StartAuthorities(); err != nil {
		return nil, "", err
	}
	for _, authority := range response.Authorities {
		if err = authority(&b); err != nil {
			return nil, "", err
		}
	}
	if err = b.StartAdditionals(); err != nil {
		return nil, "", err
	}
	for _, additionals := range response.Additionals {
		if err = additionals(&b); err != nil {
			return nil, "", err
		}
	}
	if responseBytes, err = b.Finish(); err != nil {
		return nil, "", err
	}
	return responseBytes, logMessage, nil
}

func (x *Xip) processQuestion(q dnsmessage.Question, srcAddr net.IP) (response Response, logMessage string, err error) {
	logMessage = q.Type.String() + " " + q.Name.String() + " ? "
	response = Response{
		Header: dnsmessage.Header{
			ID:                 0, // this will later be replaced with query.ID
			Response:           true,
			OpCode:             0,
			Authoritative:      true, // We're able to white label domains by always being authoritative
			Truncated:          false,
			RecursionDesired:   false,                   // this will later be replaced with query.RecursionDesired
			RecursionAvailable: false,                   // We are not recursing servers, so recursion is never available. Prevents DDOS
			RCode:              dnsmessage.RCodeSuccess, // assume success, may be replaced later
		},
	}
	if IsAcmeChallenge(q.Name.String()) && !x.blocklist(q.Name.String()) { // thanks @NormanR
		// delegate everything to its stripped (remove "_acme-challenge.") address, e.g.
		// dig _acme-challenge.127-0-0-1.sslip.io mx → NS 127-0-0-1.sslip.io
		response.Header.Authoritative = false // we're delegating, so we're not authoritative
		return x.NSResponse(q.Name, response, logMessage)
	}
	switch q.Type {
	case dnsmessage.TypeA:
		{
			return x.nameToAwithBlocklist(q, response, logMessage)
		}
	case dnsmessage.TypeAAAA:
		{
			return x.nameToAAAAwithBlocklist(q, response, logMessage)
		}
	case dnsmessage.TypeALL:
		{
			// We don't implement type ANY, so return "NotImplemented" like CloudFlare (1.1.1.1)
			// https://blog.cloudflare.com/rfc8482-saying-goodbye-to-any/
			// Google (8.8.8.8) returns every record they can find (A, AAAA, SOA, NS, MX, ...).
			response.Header.RCode = dnsmessage.RCodeNotImplemented
			return response, logMessage + "NotImplemented", nil
		}
	case dnsmessage.TypeCNAME:
		{
			// If there is a CNAME, there can only be 1, and only from Customizations
			var cname *dnsmessage.CNAMEResource
			cname = CNAMEResource(q.Name.String())
			if cname == nil {
				// No Answers, only 1 Authorities
				soaHeader, soaResource := SOAAuthority(q.Name)
				response.Authorities = append(response.Authorities,
					func(b *dnsmessage.Builder) error {
						if err = b.SOAResource(soaHeader, soaResource); err != nil {
							return err
						}
						return nil
					})
				return response, logMessage + "nil, SOA " + soaLogMessage(soaResource), nil
			}
			x.Metrics.AnsweredQueries++
			response.Answers = append(response.Answers,
				// 1 CNAME record, via Customizations
				func(b *dnsmessage.Builder) error {
					err = b.CNAMEResource(dnsmessage.ResourceHeader{
						Name:   q.Name,
						Type:   dnsmessage.TypeCNAME,
						Class:  dnsmessage.ClassINET,
						TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
						Length: 0,
					}, *cname)
					if err != nil {
						return err
					}
					return nil
				})
			return response, logMessage + cname.CNAME.String(), nil
		}
	case dnsmessage.TypeMX:
		{
			mailExchangers := MXResources(q.Name.String())
			var logMessages []string

			// We can be sure that len(mailExchangers) > 1, but we check anyway
			if len(mailExchangers) == 0 {
				return response, "", errors.New("no MX records, but there should be one")
			}
			x.Metrics.AnsweredQueries++
			response.Answers = append(response.Answers,
				// 1 or more A records; A records > 1 only available via Customizations
				func(b *dnsmessage.Builder) error {
					for _, mailExchanger := range mailExchangers {
						err = b.MXResource(dnsmessage.ResourceHeader{
							Name:   q.Name,
							Type:   dnsmessage.TypeMX,
							Class:  dnsmessage.ClassINET,
							TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
							Length: 0,
						}, mailExchanger)
					}
					if err != nil {
						return err
					}
					return nil
				})
			for _, mailExchanger := range mailExchangers {
				logMessages = append(logMessages, strconv.Itoa(int(mailExchanger.Pref))+" "+mailExchanger.MX.String())
			}
			return response, logMessage + strings.Join(logMessages, ", "), nil
		}
	case dnsmessage.TypeNS:
		{
			return x.NSResponse(q.Name, response, logMessage)
		}
	case dnsmessage.TypeSOA:
		{
			x.Metrics.AnsweredQueries++
			soaResource := SOAResource(q.Name)
			response.Answers = append(response.Answers,
				func(b *dnsmessage.Builder) error {
					err = b.SOAResource(dnsmessage.ResourceHeader{
						Name:   q.Name,
						Type:   dnsmessage.TypeSOA,
						Class:  dnsmessage.ClassINET,
						TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
						Length: 0,
					}, soaResource)
					if err != nil {
						return err
					}
					return nil
				})
			return response, logMessage + soaLogMessage(soaResource), nil
		}
	case dnsmessage.TypeTXT:
		{
			// if it's an "_acme-challenge." TXT, we return no answer but an NS authority & not authoritative
			// if it's customized records, we return them in the Answers
			// otherwise we return no Answers and Authorities SOA
			if IsAcmeChallenge(q.Name.String()) {
				// No Answers, Not Authoritative, Authorities contain NS records
				response.Header.Authoritative = false
				nameServers := x.NSResources(q.Name.String())
				var logMessages []string
				for _, nameServer := range nameServers {
					response.Authorities = append(response.Authorities,
						// 1 or more A records; A records > 1 only available via Customizations
						func(b *dnsmessage.Builder) error {
							err = b.NSResource(dnsmessage.ResourceHeader{
								Name:   q.Name,
								Type:   dnsmessage.TypeNS,
								Class:  dnsmessage.ClassINET,
								TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
								Length: 0,
							}, nameServer)
							if err != nil {
								return err
							}
							return nil
						})
					logMessages = append(logMessages, nameServer.NS.String())
				}
				return response, logMessage + "nil, NS " + strings.Join(logMessages, ", "), nil
			}
			var txts []dnsmessage.TXTResource
			txts, err = x.TXTResources(q.Name.String(), srcAddr)
			if err != nil {
				return response, "", err
			}
			if len(txts) > 0 {
				x.Metrics.AnsweredQueries++
			}
			response.Answers = append(response.Answers,
				// 1 or more TXT records via Customizations
				// Technically there can be more than one TXT record, but practically there can only be one record
				// but with multiple strings
				func(b *dnsmessage.Builder) error {
					for _, txt := range txts {
						err = b.TXTResource(dnsmessage.ResourceHeader{
							Name:   q.Name,
							Type:   dnsmessage.TypeTXT,
							Class:  dnsmessage.ClassINET,
							TTL:    180, // 3 minutes to allow key-value to propagate
							Length: 0,
						}, txt)
						if err != nil {
							return err
						}
					}
					return nil
				})
			var logMessageTXTss []string
			for _, txt := range txts {
				var logMessageTXTs []string
				for _, TXTstring := range txt.TXT {
					logMessageTXTs = append(logMessageTXTs, TXTstring)
				}
				logMessageTXTss = append(logMessageTXTss, `["`+strings.Join(logMessageTXTs, `", "`)+`"]`)
			}
			if len(logMessageTXTss) == 0 {
				return response, logMessage + "nil, SOA " + soaLogMessage(SOAResource(q.Name)), nil
			}
			return response, logMessage + strings.Join(logMessageTXTss, ", "), nil
		}
	default:
		{
			// default is the same case as an A/AAAA record which is not found,
			// i.e. we return no answers, but we return an authority section
			// No Answers, only 1 Authorities
			soaHeader, soaResource := SOAAuthority(q.Name)
			response.Authorities = append(response.Authorities,
				func(b *dnsmessage.Builder) error {
					if err = b.SOAResource(soaHeader, soaResource); err != nil {
						return err
					}
					return nil
				})
			return response, logMessage + "nil, SOA " + soaLogMessage(soaResource), nil
		}
	}
}

// NSResponse sets the Answers/Authorities depending whether we're delegating or authoritative
// (whether it's an "_acme-challenge." domain or not). Either way, it supplies the Additionals
// (IP addresses of the nameservers).
func (x *Xip) NSResponse(name dnsmessage.Name, response Response, logMessage string) (Response, string, error) {
	nameServers := x.NSResources(name.String())
	var logMessages []string
	if response.Header.Authoritative {
		// we're authoritative, so we reply with the answers
		response.Answers = append(response.Answers,
			func(b *dnsmessage.Builder) error {
				for _, nameServer := range nameServers {
					err := b.NSResource(dnsmessage.ResourceHeader{
						Name:   name,
						Type:   dnsmessage.TypeNS,
						Class:  dnsmessage.ClassINET,
						TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
						Length: 0,
					}, nameServer)
					if err != nil {
						return err
					}
				}
				return nil
			})
	} else {
		// we're NOT authoritative, so we reply who is authoritative
		response.Authorities = append(response.Authorities,
			func(b *dnsmessage.Builder) error {
				for _, nameServer := range nameServers {
					err := b.NSResource(dnsmessage.ResourceHeader{
						Name:   name,
						Type:   dnsmessage.TypeNS,
						Class:  dnsmessage.ClassINET,
						TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
						Length: 0,
					}, nameServer)
					if err != nil {
						return err
					}
				}
				return nil
			})
		logMessage += "nil, NS " // we're not supplying an answer; we're supplying the NS record that's authoritative
	}
	response.Additionals = append(response.Additionals,
		func(b *dnsmessage.Builder) error {
			for _, nameServer := range nameServers {
				for _, aResource := range NameToA(nameServer.NS.String()) {
					err := b.AResource(dnsmessage.ResourceHeader{
						Name:   nameServer.NS,
						Type:   dnsmessage.TypeA,
						Class:  dnsmessage.ClassINET,
						TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
						Length: 0,
					}, aResource)
					if err != nil {
						return err
					}
				}
				for _, aaaaResource := range NameToAAAA(nameServer.NS.String()) {
					err := b.AAAAResource(dnsmessage.ResourceHeader{
						Name:   nameServer.NS,
						Type:   dnsmessage.TypeAAAA,
						Class:  dnsmessage.ClassINET,
						TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
						Length: 0,
					}, aaaaResource)
					if err != nil {
						return err
					}
				}
			}
			return nil
		})
	for _, nameServer := range nameServers {
		logMessages = append(logMessages, nameServer.NS.String())
	}
	return response, logMessage + strings.Join(logMessages, ", "), nil
}

// NameToA returns an []AResource that matched the hostname; it returns an
// array of zero-or-one records
func NameToA(fqdnString string) []dnsmessage.AResource {
	fqdn := []byte(fqdnString)
	// is it a customized A record? If so, return early
	if domain, ok := Customizations[strings.ToLower(fqdnString)]; ok && len(domain.A) > 0 {
		return domain.A
	}
	for _, ipv4RE := range []*regexp.Regexp{ipv4REDashes, ipv4REDots} {
		if ipv4RE.Match(fqdn) {
			match := string(ipv4RE.FindSubmatch(fqdn)[2])
			match = strings.Replace(match, "-", ".", -1)
			ipv4address := net.ParseIP(match).To4()
			// We shouldn't reach here because `match` should always be valid, but we're not optimists
			if ipv4address == nil {
				// e.g. "ubuntu20.04.235.249.181-notify.sslip.io."
				log.Printf("----> Should be valid A but isn't: %s\n", fqdn) // TODO: delete this
				return []dnsmessage.AResource{}
			}
			return []dnsmessage.AResource{
				{A: [4]byte{ipv4address[0], ipv4address[1], ipv4address[2], ipv4address[3]}},
			}
		}
	}
	return []dnsmessage.AResource{}
}

// NameToAAAA returns an []AAAAResource that matched the hostname
func NameToAAAA(fqdnString string) []dnsmessage.AAAAResource {
	fqdn := []byte(fqdnString)
	// is it a customized AAAA record? If so, return early
	if domain, ok := Customizations[strings.ToLower(fqdnString)]; ok && len(domain.AAAA) > 0 {
		return domain.AAAA
	}
	if !ipv6RE.Match(fqdn) {
		return []dnsmessage.AAAAResource{}
	}

	ipv6RE.Longest()
	match := string(ipv6RE.FindSubmatch(fqdn)[2])
	match = strings.Replace(match, "-", ":", -1)
	ipv16address := net.ParseIP(match).To16()
	if ipv16address == nil {
		// We shouldn't reach here because `match` should always be valid, but we're not optimists
		log.Printf("----> Should be valid AAAA but isn't: %s\n", fqdn) // TODO: delete this
		return []dnsmessage.AAAAResource{}
	}

	AAAAR := dnsmessage.AAAAResource{}
	for i := range ipv16address {
		AAAAR.AAAA[i] = ipv16address[i]
	}
	return []dnsmessage.AAAAResource{AAAAR}
}

// CNAMEResource returns the CNAME via Customizations, otherwise nil
func CNAMEResource(fqdnString string) *dnsmessage.CNAMEResource {
	if domain, ok := Customizations[strings.ToLower(fqdnString)]; ok && domain.CNAME != (dnsmessage.CNAMEResource{}) {
		return &domain.CNAME
	}
	return nil
}

// MXResources returns either 1 or more MX records set via Customizations or
// an MX record pointing to the queried record
func MXResources(fqdnString string) []dnsmessage.MXResource {
	if domain, ok := Customizations[strings.ToLower(fqdnString)]; ok && len(domain.MX) > 0 {
		return domain.MX
	}
	mx, _ := dnsmessage.NewName(fqdnString)
	return []dnsmessage.MXResource{
		{
			Pref: 0,
			MX:   mx,
		},
	}
}

func IsAcmeChallenge(fqdnString string) bool {
	if dns01ChallengeRE.MatchString(fqdnString) {
		ipv4s := NameToA(fqdnString)
		ipv6s := NameToAAAA(fqdnString)
		if len(ipv4s) > 0 || len(ipv6s) > 0 {
			return true
		}
	}
	return false
}

func (x *Xip) NSResources(fqdnString string) []dnsmessage.NSResource {
	if x.blocklist(fqdnString) {
		x.Metrics.AnsweredQueries++
		x.Metrics.AnsweredBlockedQueries++
		return NameServers
	}
	if IsAcmeChallenge(fqdnString) {
		x.Metrics.AnsweredNSDNS01ChallengeQueries++
		strippedFqdn := dns01ChallengeRE.ReplaceAllString(fqdnString, "")
		ns, _ := dnsmessage.NewName(strippedFqdn)
		return []dnsmessage.NSResource{{NS: ns}}
	}
	x.Metrics.AnsweredQueries++
	return NameServers
}

// TXTResources returns TXT records from Customizations or KvCustomizations
func (x *Xip) TXTResources(fqdn string, ip net.IP) ([]dnsmessage.TXTResource, error) {
	if kvRE.MatchString(fqdn) {
		return x.kvTXTResources(fqdn)
	}
	if domain, ok := Customizations[strings.ToLower(fqdn)]; ok {
		// Customizations[strings.ToLower(fqdn)] returns a _function_,
		// we call that function, which has the same return signature as this method
		if domain.TXT != nil {
			return domain.TXT(x, ip)
		}
	}
	return nil, nil
}

func SOAAuthority(name dnsmessage.Name) (dnsmessage.ResourceHeader, dnsmessage.SOAResource) {
	return dnsmessage.ResourceHeader{
		Name:   name,
		Type:   dnsmessage.TypeSOA,
		Class:  dnsmessage.ClassINET,
		TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; it's not gonna change
		Length: 0,
	}, SOAResource(name)
}

// SOAResource returns the hard-coded (except MNAME) SOA
func SOAResource(name dnsmessage.Name) dnsmessage.SOAResource {
	return dnsmessage.SOAResource{
		NS:     name,
		MBox:   mbox,
		Serial: 2022020800,
		// cribbed the Refresh/Retry/Expire from google.com.
		// MinTTL was 300, but I dropped to 180 for faster
		// key-value propagation
		Refresh: 900,
		Retry:   900,
		Expire:  1800,
		MinTTL:  180,
	}
}

// when TXT for "ip.sslip.io" is queried, return the IP address of the querier
func ipSslipIo(x *Xip, srcAddr net.IP) ([]dnsmessage.TXTResource, error) {
	x.Metrics.AnsweredTXTSrcIPQueries++
	return []dnsmessage.TXTResource{{TXT: []string{srcAddr.String()}}}, nil
}

// when TXT for "metrics.sslip.io" is queried, return the cumulative metrics
func metricsSslipIo(x *Xip, _ net.IP) (txtResources []dnsmessage.TXTResource, err error) {
	<-x.DnsAmplificationAttackDelay
	var metrics []string
	uptime := time.Since(x.Metrics.Start)
	metrics = append(metrics, fmt.Sprintf("Uptime (seconds): %.0f", uptime.Seconds()))
	keyValueStore := "etcd"
	if x.isEtcdNil() {
		keyValueStore = "builtin"
	}
	metrics = append(metrics, "Key-value store: "+keyValueStore)
	metrics = append(metrics, fmt.Sprintf("Blocklist: %s %d,%d",
		x.BlocklistUpdated.Format("2006-01-02 15:04:05-07"),
		len(x.BlocklistStrings),
		len(x.BlocklistCDIRs)))
	metrics = append(metrics, fmt.Sprintf("Queries: %d", x.Metrics.Queries))
	metrics = append(metrics, fmt.Sprintf("Queries/second: %.1f", float64(x.Metrics.Queries)/uptime.Seconds()))
	metrics = append(metrics, fmt.Sprintf("AnsQueries: %d", x.Metrics.AnsweredQueries))
	metrics = append(metrics, fmt.Sprintf("AnsQueries/second: %.1f", float64(x.Metrics.AnsweredQueries)/uptime.Seconds()))
	metrics = append(metrics, fmt.Sprintf("AnsA: %d", x.Metrics.AnsweredAQueries))
	metrics = append(metrics, fmt.Sprintf("AnsAAAA: %d", x.Metrics.AnsweredAAAAQueries))
	metrics = append(metrics, fmt.Sprintf("Source IP TXT: %d", x.Metrics.AnsweredTXTSrcIPQueries))
	metrics = append(metrics, fmt.Sprintf("Version TXT: %d", x.Metrics.AnsweredXTVersionQueries))
	metrics = append(metrics, fmt.Sprintf("DNS-01 challenge: %d", x.Metrics.AnsweredNSDNS01ChallengeQueries))
	metrics = append(metrics, fmt.Sprintf("Blocked: %d", x.Metrics.AnsweredBlockedQueries))
	for _, metric := range metrics {
		txtResources = append(txtResources, dnsmessage.TXTResource{TXT: []string{metric}})
	}
	return txtResources, nil
}

// when TXT for "k-v.io" is queried, return the key-value pair
func (x *Xip) kvTXTResources(fqdn string) ([]dnsmessage.TXTResource, error) {
	// "labels" => official RFC 1035 term
	// k-v.io. => ["k-v", "io"] are labels
	var (
		verb  string // i.e. "get", "put", "delete"
		key   string // e.g. "my-key" as in "my-key.k-v.io"
		value string // e.g. "my-value" as in "put.my-value.my-key.k-v.io"
	)
	labels := strings.Split(fqdn, ".")
	labels = labels[:len(labels)-3] // strip ".k-v.io"
	// key is always present, always first subdomain of "k-v.io"
	key = strings.ToLower(labels[len(labels)-1])
	switch {
	case len(labels) == 1:
		verb = "get" // default action if only key, not verb, is not present
	case len(labels) == 2:
		verb = strings.ToLower(labels[0]) // verb, if present, is leftmost, "put.value.key.k-v.io"
	case len(labels) > 2:
		verb = strings.ToLower(labels[0])
		// concatenate multiple labels to create value, especially useful for version numbers
		value = strings.Join(labels[1:len(labels)-1], ".") // e.g. "put.94.0.2.firefox-version.k-v.io"
	}
	// prepare to query etcd:
	switch verb {
	case "get":
		return x.getKv(key)
	case "put":
		if len(labels) == 2 {
			return []dnsmessage.TXTResource{{[]string{"422: missing a value: put.value.key.k-v.io"}}}, nil
		}
		return x.putKv(key, value)
	case "delete":
		return x.deleteKv(key)
	}
	return []dnsmessage.TXTResource{{[]string{"422: valid verbs are get, put, delete"}}}, nil
}

func (x *Xip) getKv(key string) ([]dnsmessage.TXTResource, error) {
	if x.isEtcdNil() {
		if txtRecord, ok := TxtKvCustomizations[key]; ok {
			return txtRecord, nil
		}
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), etcdContextTimeout)
	defer cancel()
	resp, err := x.Etcd.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf(`couldn't GET "%s": %w`, key, err)
	}
	if len(resp.Kvs) > 0 {
		return []dnsmessage.TXTResource{{[]string{string(resp.Kvs[0].Value)}}}, nil
	}
	return []dnsmessage.TXTResource{}, nil
}

func (x *Xip) putKv(key, value string) ([]dnsmessage.TXTResource, error) {
	if len(value) > 63 { // too-long TXT records can be used in DNS amplification attacks; Truncate!
		value = value[:63]
	}
	if x.isEtcdNil() {
		TxtKvCustomizations[key] = []dnsmessage.TXTResource{
			{
				[]string{value},
			},
		}
		return TxtKvCustomizations[key], nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), etcdContextTimeout)
	defer cancel()
	_, err := x.Etcd.Put(ctx, key, value)
	if err != nil {
		return nil, fmt.Errorf("couldn't PUT (%s: %s): %w", key, value, err)
	}
	return []dnsmessage.TXTResource{{[]string{value}}}, nil
}

func (x *Xip) deleteKv(key string) ([]dnsmessage.TXTResource, error) {
	if x.isEtcdNil() {
		if _, ok := TxtKvCustomizations[key]; ok {
			delete(TxtKvCustomizations, key)
		}
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), etcdContextTimeout)
	defer cancel()
	_, err := x.Etcd.Delete(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("couldn't DELETE (key %s): %w", key, err)
	}
	return nil, nil
}

// soaLogMessage returns an easy-to-read string for logging SOA Answers/Authorities
func soaLogMessage(soaResource dnsmessage.SOAResource) string {
	return soaResource.NS.String() + " " +
		soaResource.MBox.String() + " " +
		strconv.Itoa(int(soaResource.Serial)) + " " +
		strconv.Itoa(int(soaResource.Refresh)) + " " +
		strconv.Itoa(int(soaResource.Retry)) + " " +
		strconv.Itoa(int(soaResource.Expire)) + " " +
		strconv.Itoa(int(soaResource.MinTTL))
}

// MostlyEquals compares all fields except `Start` (timestamp)
func (a Metrics) MostlyEquals(b Metrics) bool {
	if a.Queries == b.Queries &&
		a.AnsweredQueries == b.AnsweredQueries &&
		a.AnsweredAQueries == b.AnsweredAQueries &&
		a.AnsweredAAAAQueries == b.AnsweredAAAAQueries &&
		a.AnsweredTXTSrcIPQueries == b.AnsweredTXTSrcIPQueries &&
		a.AnsweredXTVersionQueries == b.AnsweredXTVersionQueries &&
		a.AnsweredNSDNS01ChallengeQueries == b.AnsweredNSDNS01ChallengeQueries &&
		a.AnsweredBlockedQueries == b.AnsweredBlockedQueries {
		return true
	}
	return false
}

// readBlocklist downloads the blocklist of domains & CIDRs that are forbidden
// because they're used for phishing (e.g. "raiffeisen")
func readBlocklist(blocklistURL string) (blocklistStrings []string, blocklistCIDRs []net.IPNet, err error) {
	resp, err := http.Get(blocklistURL)
	if err != nil {
		log.Println(fmt.Errorf(`failed to download blocklist "%s": %w`, blocklistURL, err))
	} else {
		defer resp.Body.Close()
		if resp.StatusCode > 299 {
			log.Printf(`failed to download blocklist "%s", HTTP status: "%d"`, blocklistURL, resp.StatusCode)
		} else {
			blocklistStrings, blocklistCIDRs, err = ReadBlocklist(resp.Body)
			if err != nil {
				log.Println(fmt.Errorf(`failed to parse blocklist "%s": %w`, blocklistURL, err))
			}
		}
	}
	return blocklistStrings, blocklistCIDRs, err
}

// ReadBlocklist "sanitizes" the block list, removing comments, invalid characters
// and lowercasing the names to be blocked
func ReadBlocklist(blocklist io.Reader) (stringBlocklists []string, cidrBlocklists []net.IPNet, err error) {
	scanner := bufio.NewScanner(blocklist)
	comments := regexp.MustCompile(`#.*`)
	invalidDNSchars := regexp.MustCompile(`[^-_0-9a-z]`)
	invalidDNScharsWithSlashesDotsAndColons := regexp.MustCompile(`[^-_0-9a-z/.:]`)

	for scanner.Scan() {
		line := scanner.Text()
		line = strings.ToLower(line)
		line = comments.ReplaceAllString(line, "")                                // strip comments
		line = invalidDNScharsWithSlashesDotsAndColons.ReplaceAllString(line, "") // strip invalid characters
		_, ipcidr, err := net.ParseCIDR(line)
		if err != nil {
			line = invalidDNSchars.ReplaceAllString(line, "") // strip invalid DNS characters
			if line == "" {
				continue
			}
			stringBlocklists = append(stringBlocklists, line)
		} else {
			cidrBlocklists = append(cidrBlocklists, *ipcidr)
		}
	}
	if err = scanner.Err(); err != nil {
		return []string{}, []net.IPNet{}, err
	}
	return stringBlocklists, cidrBlocklists, nil
}

func (x *Xip) isEtcdNil() bool {
	// comparing interfaces to nil are tricky: interfaces contain both a type
	// and a value, and although the value is nil the type isn't, so we need the following
	if x.Etcd == nil || reflect.ValueOf(x.Etcd).IsNil() {
		return true
	}
	return false
}

func (x *Xip) blocklist(hostname string) bool {
	aResources := NameToA(hostname)
	aaaaResources := NameToAAAA(hostname)
	var ip net.IP
	if len(aResources) == 1 {
		ip = aResources[0].A[:]
	}
	if len(aaaaResources) == 1 {
		ip = aaaaResources[0].AAAA[:]
	}
	if len(aResources) == 0 && len(aaaaResources) == 0 {
		return false
	}
	if ip.IsPrivate() {
		return false
	}
	for _, blockstring := range x.BlocklistStrings {
		if strings.Contains(hostname, blockstring) {
			return true
		}
	}
	for _, blockCDIR := range x.BlocklistCDIRs {
		if blockCDIR.Contains(ip) {
			return true
		}
	}
	return false
}

func (x *Xip) nameToAwithBlocklist(q dnsmessage.Question, response Response, logMessage string) (_ Response, _ string, err error) {
	var nameToAs []dnsmessage.AResource
	nameToAs = NameToA(q.Name.String())
	if len(nameToAs) == 0 {
		// No Answers, only 1 Authorities
		soaHeader, soaResource := SOAAuthority(q.Name)
		response.Authorities = append(response.Authorities,
			func(b *dnsmessage.Builder) error {
				if err = b.SOAResource(soaHeader, soaResource); err != nil {
					return err
				}
				return nil
			})
		return response, logMessage + "nil, SOA " + soaLogMessage(soaResource), nil
	}
	if x.blocklist(q.Name.String()) {
		x.Metrics.AnsweredQueries++
		x.Metrics.AnsweredBlockedQueries++
		response.Answers = append(response.Answers,
			// 1 or more A records; A records > 1 only available via Customizations
			func(b *dnsmessage.Builder) error {
				err = b.AResource(dnsmessage.ResourceHeader{
					Name:   q.Name,
					Type:   dnsmessage.TypeA,
					Class:  dnsmessage.ClassINET,
					TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
					Length: 0,
				}, Customizations["ns-aws.sslip.io."].A[0])
				if err != nil {
					return err
				}
				return nil
			})
		return response, logMessage + net.IP(Customizations["ns-aws.sslip.io."].A[0].A[:]).String(), nil
	}
	x.Metrics.AnsweredQueries++
	x.Metrics.AnsweredAQueries++
	response.Answers = append(response.Answers,
		// 1 or more A records; A records > 1 only available via Customizations
		func(b *dnsmessage.Builder) error {
			for _, nameToA := range nameToAs {
				err = b.AResource(dnsmessage.ResourceHeader{
					Name:   q.Name,
					Type:   dnsmessage.TypeA,
					Class:  dnsmessage.ClassINET,
					TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
					Length: 0,
				}, nameToA)
				if err != nil {
					return err
				}
			}
			return nil
		})
	var logMessages []string
	for _, nameToA := range nameToAs {
		ip := net.IP(nameToA.A[:])
		logMessages = append(logMessages, ip.String())
	}
	return response, logMessage + strings.Join(logMessages, ", "), nil
}

func (x *Xip) nameToAAAAwithBlocklist(q dnsmessage.Question, response Response, logMessage string) (_ Response, _ string, err error) {
	var nameToAAAAs []dnsmessage.AAAAResource
	nameToAAAAs = NameToAAAA(q.Name.String())
	if len(nameToAAAAs) == 0 {
		// No Answers, only 1 Authorities
		soaHeader, soaResource := SOAAuthority(q.Name)
		response.Authorities = append(response.Authorities,
			func(b *dnsmessage.Builder) error {
				if err = b.SOAResource(soaHeader, soaResource); err != nil {
					return err
				}
				return nil
			})
		return response, logMessage + "nil, SOA " + soaLogMessage(soaResource), nil
	}
	if x.blocklist(q.Name.String()) {
		x.Metrics.AnsweredQueries++
		x.Metrics.AnsweredBlockedQueries++
		response.Answers = append(response.Answers,
			// 1 or more A records; A records > 1 only available via Customizations
			func(b *dnsmessage.Builder) error {
				err = b.AAAAResource(dnsmessage.ResourceHeader{
					Name:   q.Name,
					Type:   dnsmessage.TypeA,
					Class:  dnsmessage.ClassINET,
					TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
					Length: 0,
				}, Customizations["ns-aws.sslip.io."].AAAA[0])
				if err != nil {
					return err
				}
				return nil
			})
		return response, logMessage + net.IP(Customizations["ns-aws.sslip.io."].AAAA[0].AAAA[:]).String(), nil
	}
	x.Metrics.AnsweredQueries++
	x.Metrics.AnsweredAAAAQueries++
	response.Answers = append(response.Answers,
		// 1 or more AAAA records; AAAA records > 1 only available via Customizations
		func(b *dnsmessage.Builder) error {
			for _, nameToAAAA := range nameToAAAAs {
				err = b.AAAAResource(dnsmessage.ResourceHeader{
					Name:   q.Name,
					Type:   dnsmessage.TypeAAAA,
					Class:  dnsmessage.ClassINET,
					TTL:    604800, // 60 * 60 * 24 * 7 == 1 week; long TTL, these IP addrs don't change
					Length: 0,
				}, nameToAAAA)
				if err != nil {
					return err
				}
			}
			return nil
		})
	var logMessages []string
	for _, nameToAAAA := range nameToAAAAs {
		ip := net.IP(nameToAAAA.AAAA[:])
		logMessages = append(logMessages, ip.String())
	}
	return response, logMessage + strings.Join(logMessages, ", "), nil
}

// clientv3New attempts to connect to local etcd and retrieve a key to make
// sure the connection works. If for any reason it fails it returns nil +
// error
func clientv3New(etcdEndpoint string) (*clientv3.Client, error) {
	etcdEndpoints := []string{etcdEndpoint}
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   etcdEndpoints,
		DialTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		return nil, err
	}
	// Let's do a query to determine if etcd is really, truly there
	ctx, cancel := context.WithTimeout(context.Background(), etcdContextTimeout)
	defer cancel()
	_, err = etcdCli.Get(ctx, "some-silly-key, doesn't matter if it exists")
	if err != nil {
		return nil, err
	}
	return etcdCli, nil
}
