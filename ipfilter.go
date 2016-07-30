package ipfilter

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/mholt/caddy"
	"github.com/mholt/caddy/caddyhttp/httpserver"
	"github.com/oschwald/maxminddb-golang"
)

// IPFilter is a middleware for filtering clients based on their ip or country's ISO code.
type IPFilter struct {
	Next   httpserver.Handler
	Config IPFConfig
}

// IPPath holds the configuration of a single ipfilter block.
type IPPath struct {
	PathScopes   []string
	BlockPage    string
	CountryCodes []string
	Ranges       []Range
	IsBlock      bool
	Strict       bool
}

// IPFConfig holds the configuration for the ipfilter middleware.
type IPFConfig struct {
	Paths     []IPPath
	DBHandler *maxminddb.Reader // Database's handler if it gets opened.
}

// Range is a pair of two 'net.IP'.
type Range struct {
	start net.IP
	end   net.IP
}

// InRange is a method of 'Range' takes a pointer to net.IP, returns true if in range, false otherwise.
func (rng Range) InRange(ip *net.IP) bool {
	if bytes.Compare(*ip, rng.start) >= 0 && bytes.Compare(*ip, rng.end) <= 0 {
		return true
	}
	return false
}

// OnlyCountry is used to fetch only the country's code from 'mmdb'.
type OnlyCountry struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

// Status is used to keep track of the status of the request.
type Status struct {
	countryMatch, inRange bool
}

// Any returns 'true' if we have a match on a country code or an IP in range.
func (s *Status) Any() bool {
	return s.countryMatch || s.inRange
}

// block will take care of blocking
func block(blockPage string, w *http.ResponseWriter) (int, error) {
	if blockPage != "" {
		bp, err := os.Open(blockPage)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		defer bp.Close()

		if _, err := io.Copy(*w, bp); err != nil {
			return http.StatusInternalServerError, err
		}
		// we wrote the blockpage, return OK.
		return http.StatusOK, nil
	}

	// if we don't have blockpage, return forbidden.
	return http.StatusForbidden, nil
}

// Init initializes the plugin
func init() {
	caddy.RegisterPlugin("ipfilter", caddy.Plugin{
		ServerType: "http",
		Action:     Setup,
	})
}

// Setup parses the ipfilter configuration and returns the middleware handler.
func Setup(c *caddy.Controller) error {
	ifconfig, err := ipfilterParse(c)
	if err != nil {
		return err
	}

	// Create new middleware
	newMiddleWare := func(next httpserver.Handler) httpserver.Handler {
		return &IPFilter{
			Next:   next,
			Config: ifconfig,
		}
	}
	// Add middleware
	cfg := httpserver.GetConfig(c)
	cfg.AddMiddleware(newMiddleWare)

	return nil
}

func getClientIPs(r *http.Request, strict bool) ([]net.IP, error) {
	var ips []string

	// Use the client ip(s) from the 'X-Forwarded-For' header, if available.
	if fwdFor := r.Header.Get("X-Forwarded-For"); fwdFor != "" && !strict {
		ips = strings.Split(fwdFor, ",")
	} else {
		// Otherwise, get the client ip from the request remote address.
		var err error
		var ip string
		ip, _, err = net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return nil, err
		}
		ips = []string{ip}
	}

	// Parse each ip address string into a net.IP.
	var parsedIPs = make([]net.IP, len(ips))
	var count = 0
	for _, ip := range ips {
		parsedIP := net.ParseIP(strings.TrimSpace(ip))
		if parsedIP != nil {
			parsedIPs[count] = parsedIP
			count++
		}
	}
	if count == 0 {
		return nil, errors.New("unable to parse address")
	}

	return parsedIPs, nil
}

// ShouldAllow takes a path and a request and decides if it should be allowed
func (ipf IPFilter) ShouldAllow(path IPPath, r *http.Request) (bool, string, error) {
	allow := true
	scopeMatched := ""

	// check if we are in one of our scopes.
	for _, scope := range path.PathScopes {
		if httpserver.Path(r.URL.Path).Matches(scope) {
			// extract the client IP(s) and parse them.
			clientIPs, err := getClientIPs(r, path.Strict)
			if err != nil {
				return false, scope, err
			}

			// request status.
			var rs Status

			if len(path.CountryCodes) != 0 {
				// do the lookup.
				var result OnlyCountry
				for _, clientIP := range clientIPs {
					if err = ipf.Config.DBHandler.Lookup(clientIP, &result); err != nil {
						return false, scope, err
					}

					// get only the ISOCode out of the lookup results.
					clientCountry := result.Country.ISOCode
					for _, c := range path.CountryCodes {
						if clientCountry == c {
							rs.countryMatch = true
							break
						}
					}
					if rs.countryMatch {
						break
					}
				}
			}

			if len(path.Ranges) != 0 {
				for _, rng := range path.Ranges {
					for _, clientIP := range clientIPs {
						if rng.InRange(&clientIP) {
							rs.inRange = true
							break
						}
					}
					if rs.inRange {
						break
					}
				}
			}

			scopeMatched = scope
			if rs.Any() {
				// Rule matched, if the rule has IsBlock = true then we have to deny access
				allow = !path.IsBlock
			} else {
				// Rule did not match, if the rule has IsBlock = true then we have to allow access
				allow = path.IsBlock
			}

			// We only have to test the first path that matches because it is the most specific
			break
		}
	}

	// no scope match, pass-through.
	return allow, scopeMatched, nil
}

func (ipf IPFilter) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	allow := true
	matchedPath := ""
	blockPage := ""

	// Loop over all IPPaths in the config
	for _, path := range ipf.Config.Paths {
		pathAllow, pathMathedPath, err := ipf.ShouldAllow(path, r)
		if err != nil {
			return http.StatusInternalServerError, err
		}

		if len(pathMathedPath) >= len(matchedPath) {
			allow = pathAllow
			matchedPath = pathMathedPath
			blockPage = path.BlockPage
		}
	}

	if !allow {
		return block(blockPage, &w)
	}
	return ipf.Next.ServeHTTP(w, r)
}

// parseIP parses a string to an IP range.
func parseIP(ip string) (Range, error) {
	// check if the ip isn't complete;
	// e.g. 192.168 -> Range{"192.168.0.0", "192.168.255.255"}
	dotSplit := strings.Split(ip, ".")
	if len(dotSplit) < 4 {
		startR := make([]string, len(dotSplit), 4)
		copy(startR, dotSplit)
		for len(dotSplit) < 4 {
			startR = append(startR, "0")
			dotSplit = append(dotSplit, "255")
		}
		start := net.ParseIP(strings.Join(startR, "."))
		end := net.ParseIP(strings.Join(dotSplit, "."))
		if start.To4() == nil || end.To4() == nil {
			return Range{start, end}, errors.New("Can't parse IPv4 address")
		}

		return Range{start, end}, nil
	}

	// try to split on '-' to see if it is a range of ips e.g. 1.1.1.1-10
	splitted := strings.Split(ip, "-")
	if len(splitted) > 1 { // if more than one, then we got a range e.g. ["1.1.1.1", "10"]
		start := net.ParseIP(splitted[0])
		// make sure that we got a valid IPv4 IP.
		if start.To4() == nil {
			return Range{start, start}, errors.New("Can't parse IPv4 address")
		}

		// split the start of the range on "." and switch the last field with splitted[1], e.g 1.1.1.1 -> 1.1.1.10
		fields := strings.Split(start.String(), ".")
		fields[3] = splitted[1]
		end := net.ParseIP(strings.Join(fields, "."))

		// parse the end range.
		if end.To4() == nil {
			return Range{start, end}, errors.New("Can't parse IPv4 address")
		}

		return Range{start, end}, nil
	}

	// the IP is not a range.
	parsedIP := net.ParseIP(ip)
	if parsedIP.To4() == nil {
		return Range{parsedIP, parsedIP}, errors.New("Can't parse IPv4 address")
	}

	// return singular IPs as a range e.g Range{192.168.1.100, 192.168.1.100}
	return Range{parsedIP, parsedIP}, nil
}

// ipfilterParseSingle parses a single ipfilter {} block from the caddy config.
func ipfilterParseSingle(config *IPFConfig, c *caddy.Controller) (IPPath, error) {
	var cPath IPPath

	// Get PathScopes
	cPath.PathScopes = c.RemainingArgs()
	if len(cPath.PathScopes) == 0 {
		return cPath, c.ArgErr()
	}

	// Sort PathScopes by length (the longest is always the most specific so should be tested first)
	sort.Sort(sort.Reverse(ByLength(cPath.PathScopes)))

	for c.NextBlock() {
		value := c.Val()

		switch value {
		case "rule":
			if !c.NextArg() {
				return cPath, c.ArgErr()
			}

			rule := c.Val()
			if rule == "block" {
				cPath.IsBlock = true
			} else if rule != "allow" {
				return cPath, c.Err("ipfilter: Rule should be 'block' or 'allow'")
			}
		case "database":
			if !c.NextArg() {
				return cPath, c.ArgErr()
			}
			// Check if a database has already been opened
			if config.DBHandler != nil {
				return cPath, c.Err("ipfilter: A database is already opened")
			}

			database := c.Val()

			// Open the database.
			var err error
			config.DBHandler, err = maxminddb.Open(database)
			if err != nil {
				return cPath, c.Err("ipfilter: Can't open database: " + database)
			}
		case "blockpage":
			if !c.NextArg() {
				return cPath, c.ArgErr()
			}

			// check if blockpage exists.
			blockpage := c.Val()
			if _, err := os.Stat(blockpage); os.IsNotExist(err) {
				return cPath, c.Err("ipfilter: No such file: " + blockpage)
			}
			cPath.BlockPage = blockpage
		case "country":
			cPath.CountryCodes = c.RemainingArgs()
			if len(cPath.CountryCodes) == 0 {
				return cPath, c.ArgErr()
			}
		case "ip":
			ips := c.RemainingArgs()
			if len(ips) == 0 {
				return cPath, c.ArgErr()
			}

			for _, ip := range ips {
				ipRange, err := parseIP(ip)
				if err != nil {
					return cPath, c.Err("ipfilter: " + err.Error())
				}

				cPath.Ranges = append(cPath.Ranges, ipRange)
			}
		case "strict":
			cPath.Strict = true
		}
	}

	return cPath, nil
}

// ipfilterParse parses all ipfilter {} blocks to an IPFConfig
func ipfilterParse(c *caddy.Controller) (IPFConfig, error) {
	var config IPFConfig

	var hasCountryCodes, hasRanges bool

	for c.Next() {
		path, err := ipfilterParseSingle(&config, c)
		if err != nil {
			return config, err
		}

		if len(path.CountryCodes) != 0 {
			hasCountryCodes = true
		}
		if len(path.Ranges) != 0 {
			hasRanges = true
		}

		config.Paths = append(config.Paths, path)
	}

	// having a database is mandatory if you are blocking by country codes.
	if hasCountryCodes && config.DBHandler == nil {
		return config, c.Err("ipfilter: Database is required to block/allow by country")
	}

	// needs atleast one of the three.
	if !hasCountryCodes && !hasRanges {
		return config, c.Err("ipfilter: No IPs or Country codes has been provided")
	}

	return config, nil
}

// ByLength sorts strings by length and alphabetically (if same length)
type ByLength []string

func (s ByLength) Len() int      { return len(s) }
func (s ByLength) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s ByLength) Less(i, j int) bool {
	if len(s[i]) < len(s[j]) {
		return true
	} else if len(s[i]) == len(s[j]) {
		return s[i] < s[j] // Compare alphabetically in ascending order
	}
	return false
}
