#!/usr/bin/env ruby
#
# DOMAIN=sslip.io rspec --format documentation --color spec
#
# Admittedly it's overkill to use rspec to run a set of assertions
# against a DNS server -- a simple shell script would have been
# shorter and more understandable. We are using rspec merely to
# practice using rspec.
def get_whois_nameservers(domain)
  whois_output = `whois #{domain}`
  soa = nil
  whois_lines = whois_output.split(/\n+/)
  nameserver_lines = whois_lines.select { |line| line =~ /^Name Server:/ }
  nameservers = nameserver_lines.map { |line| line.split.last.downcase }.uniq
  # whois records don't have trail '.'; NS records do; add trailing '.'
  nameservers.map { |ns| ns << '.' }
  nameservers
end

domain = ENV['DOMAIN'] || 'example.com'
sslip_version = '2.6.1'
whois_nameservers = get_whois_nameservers(domain)

describe domain do
  soa = nil

  context "when evaluating $DOMAIN (\"#{domain}\") environment variable" do
    let (:domain) { ENV['DOMAIN'] }
    it 'is set' do
      expect(domain).not_to be_nil
    end
    it 'is not an empty string' do
      expect(domain).not_to eq('')
    end
  end

  it "should have at least 2 nameservers" do
    expect(whois_nameservers.size).to be > 1
  end

  whois_nameservers.each do |whois_nameserver|
    it "nameserver #{whois_nameserver}'s NS records match whois's #{whois_nameservers}, " +
      "`dig @#{whois_nameserver} ns #{domain} +short`" do
      dig_nameservers = `dig @#{whois_nameserver} ns #{domain} +short`.split(/\n+/)
      expect(dig_nameservers.sort).to eq(whois_nameservers.sort)
    end

    it "nameserver #{whois_nameserver}'s SOA record match" do
      dig_soa = `dig @#{whois_nameserver} soa #{domain} +short`
      soa = soa || dig_soa
      expect(dig_soa).to eq(soa)
    end

    it "nameserver #{whois_nameserver}'s has an A record" do
      expect(`dig @#{whois_nameserver} a #{domain} +short`.chomp).not_to eq('')
      expect($?.success?).to be true
    end

    it "nameserver #{whois_nameserver}'s has an AAAA record" do
      expect(`dig @#{whois_nameserver} a #{domain} +short`.chomp).not_to eq('')
      expect($?.success?).to be true
    end

    a = [ rand(256), rand(256), rand(256), rand(256) ]
    it "resolves #{a.join(".")}.#{domain} to #{a.join(".")}" do
      expect(`dig @#{whois_nameserver} #{a.join(".") + "." + domain} +short`.chomp).to  eq(a.join("."))
    end

    a = [ rand(256), rand(256), rand(256), rand(256) ]
    it "resolves #{a.join("-")}.#{domain} to #{a.join(".")}" do
      expect(`dig @#{whois_nameserver} #{a.join("-") + "." + domain} +short`.chomp).to  eq(a.join("."))
    end

    a = [ rand(256), rand(256), rand(256), rand(256) ]
    b = [ ('a'..'z').to_a, ('0'..'9').to_a ].flatten.shuffle[0,8].join
    it "resolves #{b}.#{a.join("-")}.#{domain} to #{a.join(".")}" do
      expect(`dig @#{whois_nameserver} #{b}.#{a.join("-") + "." + domain} +short`.chomp).to  eq(a.join("."))
    end

    a = [ rand(256), rand(256), rand(256), rand(256) ]
    b = [ ('a'..'z').to_a, ('0'..'9').to_a ].flatten.shuffle[0,8].join
    it "resolves #{a.join("-")}.#{b} to #{a.join(".")}" do
      expect(`dig @#{whois_nameserver} #{a.join("-") + "." + b} +short`.chomp).to  eq(a.join("."))
    end

    # don't begin the hostname with a double-dash -- `dig` mistakes it for an argument
    it "resolves api.--.#{domain}' to eq ::)}" do
      expect(`dig @#{whois_nameserver} AAAA api.--.#{domain} +short`.chomp).to eq("::")
    end

    it "resolves localhost.--1.#{domain}' to eq ::1)}" do
      expect(`dig @#{whois_nameserver} AAAA localhost.api.--1.#{domain} +short`.chomp).to eq("::1")
    end

    it "resolves 2001-4860-4860--8888.#{domain}' to eq 2001:4860:4860::8888)}" do
      expect(`dig @#{whois_nameserver} AAAA 2001-4860-4860--8888.#{domain} +short`.chomp).to eq("2001:4860:4860::8888")
    end

    it "resolves 2601-646-100-69f0--24.#{domain}' to eq 2601:646:100:69f0::24)}" do
      expect(`dig @#{whois_nameserver} AAAA 2601-646-100-69f0--24.#{domain} +short`.chomp).to eq("2601:646:100:69f0::24")
    end

    it "gets the expected version number, #{sslip_version}" do
      expect(`dig @#{whois_nameserver} TXT version.status.#{domain} +short`).to include(sslip_version)
    end

    it "gets the source (querier's) IP address" do
      # Look on my Regular Expressions, ye mighty, and despair!
      expect(`dig @#{whois_nameserver} TXT ip.#{domain} +short`).to match(/^"(\d+\.\d+\.\d+\.\d+)|(([[:xdigit:]]*:){2,7}[[:xdigit:]]*)"$/)
    end

    context "k-v.io tested on the #{whois_nameserver} nameserver" do
      seconds_since_epoch = Time.now.to_i
      it "sets a value, #{seconds_since_epoch}, on the key sslipio-spec.k-v.io" do
        expect(`dig @#{whois_nameserver} put.#{seconds_since_epoch}.sslipio-spec.k-v.io TXT +short`).to match(/^"#{seconds_since_epoch}"$/)
        sleep 5 # give it time to propagate
      end

      it "gets the newly-set value, #{seconds_since_epoch}, from the key, sslipio-spec.k-v.io" do
        whois_nameservers.each do |ns|
          expect(`dig @#{ns} sslipio-spec.k-v.io TXT +short`).to match(/^\"#{seconds_since_epoch}\"$/)
        end
      end

      it "deletes the value from the key sslipio-spec.k-v.io" do
        expect(`dig @#{whois_nameserver} delete.sslipio-spec.k-v.io TXT +short`).to match(/^$/)
        sleep 5 # give it time to propagate
      end

      it "gets no value from the freshly-deleted key, sslipio-spec.k-v.io" do
        whois_nameservers.each do |ns|
          expect(`dig @#{ns} sslipio-spec.k-v.io TXT +short`).to match(/^$/)
        end
      end
    end

    # check the website
    it "is able to reach https://#{domain} and get a valid response (2xx)" do
      `curl -If https://#{domain} 2> /dev/null`
      expect($?.success?).to be true
    end
  end
end
