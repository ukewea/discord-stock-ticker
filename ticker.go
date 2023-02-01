package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/rssnyder/discord-stock-ticker/utils"
)

type Ticker struct {
	Ticker         string        `json:"ticker"`
	Name           string        `json:"name"`
	Nickname       bool          `json:"nickname"`
	Frequency      int           `json:"frequency"`
	Color          bool          `json:"color"`
	Decorator      string        `json:"decorator"`
	Currency       string        `json:"currency"`
	CurrencySymbol string        `json:"currency_symbol"`
	Decimals       int           `json:"decimals"`
	Activity       string        `json:"activity"`
	Pair           string        `json:"pair"`
	PairFlip       bool          `json:"pair_flip"`
	Multiplier     int           `json:"multiplier"`
	ClientID       string        `json:"client_id"`
	Crypto         bool          `json:"crypto"`
	Token          string        `json:"discord_bot_token"`
	TwelveDataKey  string        `json:"twelve_data_key"`
	Exrate         float64       `json:"exrate"`
	Close          chan struct{} `json:"-"`
}

// label returns a human readble id for this bot
func (s *Ticker) label() string {
	var label string
	if s.Crypto {
		label = strings.ToLower(fmt.Sprintf("%s-%s", s.Name, s.Currency))
	} else {
		label = strings.ToLower(fmt.Sprintf("%s-%s", s.Ticker, s.Currency))
	}
	if len(label) > 32 {
		label = label[:32]
	}
	return label
}

func (s *Ticker) watchStockPrice() {

	// create a new discord session using the provided bot token.
	dg, err := discordgo.New("Bot " + s.Token)
	if err != nil {
		logger.Errorf("Creating Discord session: %s", err)
		lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Ticker, "guild": "None"}).Set(0)
		return
	}

	// show as online
	err = dg.Open()
	if err != nil {
		logger.Errorf("Opening discord connection: %s", err)
		lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Ticker, "guild": "None"}).Set(0)
		return
	}

	// Get guides for bot
	guilds, err := dg.UserGuilds(100, "", "")
	if err != nil {
		logger.Errorf("Getting guilds: %s", err)
		s.Nickname = false
	}
	if len(guilds) == 0 {
		s.Nickname = false
	}

	// check for frequency override
	if *frequency != 0 {
		s.Frequency = *frequency
	}

	// If other currency, get rate
	if s.Currency != "USD" {
		exData, err := utils.GetStockPrice(s.Currency + "=X")
		if err != nil {
			logger.Errorf("Unable to fetch exchange rate for %s, default to USD.", s.Currency)
		} else {
			if len(exData.QuoteSummary.Results) > 0 {
				s.Exrate = exData.QuoteSummary.Results[0].Price.RegularMarketPrice.Raw * float64(s.Multiplier)
			} else {
				logger.Errorf("Bad exchange rate for %s, default to USD.", s.Currency)
			}
		}
	}

	// Set arrows if no custom decorator
	var arrows bool
	if s.Decorator == "" {
		arrows = true
	}

	// Grab custom activity messages
	var custom_activity []string
	itr := 0
	itrSeed := 0.0
	if s.Activity != "" {
		custom_activity = strings.Split(s.Activity, ";")
	}

	// perform management operations
	if *managed {
		setName(dg, s.label())
	}

	logger.Infof("Watching stock price for %s", s.Ticker)
	ticker := time.NewTicker(time.Duration(s.Frequency) * time.Second)

	// continuously watch
	for {
		select {
		case <-s.Close:
			logger.Infof("Shutting down price watching for %s", s.Name)
			return
		case <-ticker.C:
			logger.Debugf("Fetching stock price for %s", s.Name)

			var fmtPrice string
			var fmtDiffPercent string
			var diffPercent float64
			var fmtDiffChange string
			var marketState string
			// use twelve data as source
			if s.TwelveDataKey != "" {
				priceDataTS, err := utils.GetTimeSeries(s.Ticker, "min", s.TwelveDataKey)
				if err != nil {
					logger.Errorf("Unable to fetch twelvedata stock price for %s", s.Name)
					continue
				}
				if len(priceDataTS.Values) == 0 {
					logger.Errorf("Unable to fetch twelvedata stock price for %s", s.Name)
					continue
				}

				fmtPrice = priceDataTS.Values[0].Close

				priceDataDay, err := utils.GetTimeSeries(s.Ticker, "day", s.TwelveDataKey)
				if err != nil {
					logger.Errorf("Unable to fetch twelvedata stock price for %s", s.Name)
					continue
				}
				if len(priceDataDay.Values) == 0 {
					logger.Errorf("Unable to fetch twelvedata stock price for %s", s.Name)
					continue
				}

				nowRaw, err := strconv.ParseFloat(fmtPrice, 64)
				if err != nil {
					logger.Errorf("Unable to deal with twelvedata ts data %s", fmtPrice)
					continue
				}

				closeRaw, err := strconv.ParseFloat(priceDataDay.Values[1].Close, 64)
				if err != nil {
					logger.Errorf("Unable to deal with twelvedata day data %s", priceDataDay.Values[0].Open)
					continue
				}

				fmtDiff := nowRaw - closeRaw
				fmtDiffChange = fmt.Sprintf("%.2f", fmtDiff)
				diffPercent = (fmtDiff / closeRaw) * 100
				fmtDiffPercent = fmt.Sprintf("%.2f%%", diffPercent)
				marketState = ""
			} else {
				// use yahoo as source
				priceData, err := utils.GetStockPrice(s.Ticker)
				if err != nil {
					logger.Errorf("Unable to fetch yahoo stock price for %s", s.Name)
					continue
				}

				if len(priceData.QuoteSummary.Results) == 0 {
					logger.Errorf("Yahoo returned bad data for %s", s.Name)
					continue
				}
				fmtPrice = priceData.QuoteSummary.Results[0].Price.RegularMarketPrice.Fmt

				// Check if conversion is needed
				if s.Exrate != 0 {
					rawPrice := s.Exrate * priceData.QuoteSummary.Results[0].Price.RegularMarketPrice.Raw
					fmtPrice = strconv.FormatFloat(rawPrice, 'f', 2, 64)
				}

				marketState = priceData.QuoteSummary.Results[0].Price.MarketState
				// check for day or after hours change
				if marketState == "POST" {
					diffPercentStruct := priceData.QuoteSummary.Results[0].Price.PostMarketChangePercent
					diffPercent = diffPercentStruct.Raw
					fmtDiffPercent = diffPercentStruct.Fmt
					fmtDiffChange = priceData.QuoteSummary.Results[0].Price.PostMarketChange.Fmt
				} else if marketState == "PRE" {
					diffPercentStruct := priceData.QuoteSummary.Results[0].Price.PreMarketChangePercent
					diffPercent = diffPercentStruct.Raw
					fmtDiffPercent = diffPercentStruct.Fmt
					fmtDiffChange = priceData.QuoteSummary.Results[0].Price.PreMarketChange.Fmt
				} else {
					diffPercentStruct := priceData.QuoteSummary.Results[0].Price.RegularMarketChangePercent
					diffPercent = diffPercentStruct.Raw
					fmtDiffPercent = diffPercentStruct.Fmt
					fmtDiffChange = priceData.QuoteSummary.Results[0].Price.RegularMarketChange.Fmt
				}
			}

			// calculate if price has moved up or down
			var increase bool
			if len(fmtDiffChange) == 0 {
				increase = true
			} else if string(fmtDiffChange[0]) == "-" {
				increase = false
			} else {
				increase = true
			}

			// add plus sign to diff percentage
			if diffPercent >= 0.0005 {
				fmtDiffPercent = "+" + fmtDiffPercent
			}

			// removes the '%' in the end (e.g., 1.1% becomes 1.1)
			// for flexibility when formatting string
			fmtDiffPercent = strings.TrimSuffix(fmtDiffPercent, "%")

			if arrows {
				s.Decorator = "⬊"
				if increase {
					s.Decorator = "⬈"
				}
			}

			if s.Nickname {
				// update nickname instead of activity
				var nickname string
				var activity string

				// format nickname, looks like "201.75 USD"
				nickname = fmt.Sprintf("%s %s", fmtPrice, strings.ToUpper(s.Currency))

				// format activity, looks like "+0.82 % | TSLA"
				// fmtDiffChange may be added to activity as well, if preferred
				if len(marketState) > 0 {
					activity = fmt.Sprintf("%s %% | %s (%s)", fmtDiffPercent, strings.ToUpper(strings.ToUpper(s.Name)), marketState)
				} else {
					activity = fmt.Sprintf("%s %% | %s", fmtDiffPercent, strings.ToUpper(strings.ToUpper(s.Name)))
				}

				// Update nickname in guilds
				for _, g := range guilds {
					err = dg.GuildMemberNickname(g.ID, "@me", nickname)
					if err != nil {
						logger.Errorf("Updating nickname: %s", err)
						continue
					}
					logger.Debugf("Set nickname in %s: %s", g.Name, nickname)
					lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Ticker, "guild": g.Name}).SetToCurrentTime()

					if s.Color {
						// change bot color
						err = setRole(dg, s.ClientID, g.ID, increase)
						if err != nil {
							logger.Errorf("Color roles: %s", err)
						}
					}

				}

				// Custom activity messages
				if len(custom_activity) > 0 {

					// Display the real activity once per cycle
					if itr == len(custom_activity) {
						itr = 0
						itrSeed = 0.0
					} else if math.Mod(itrSeed, 2.0) == 1.0 {
						activity = custom_activity[itr]
						itr++
						itrSeed++
					} else {
						activity = custom_activity[itr]
						itrSeed++
					}
				}

				err = dg.UpdateStatusComplex(*newUpdateStatusData(0, discordgo.ActivityTypeWatching, activity, ""))
				if err != nil {
					logger.Errorf("Unable to set activity: %s", err)
				} else {
					logger.Debugf("Set activity: %s", activity)
				}

			} else {
				activity := fmt.Sprintf("%s %s %s", fmtPrice, s.Decorator, fmtDiffPercent)
				err = dg.UpdateStatusComplex(*newUpdateStatusData(0, discordgo.ActivityTypeWatching, activity, ""))
				if err != nil {
					logger.Errorf("Unable to set activity: %s", err)
				} else {
					logger.Debugf("Set activity: %s", activity)
					lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Ticker, "guild": "None"}).SetToCurrentTime()
				}
			}

			time.Sleep(time.Duration(s.Frequency) * time.Second)
		}
	}
}

func (s *Ticker) watchCryptoPrice() {
	var nilCache *redis.Client

	// create a new discord session using the provided bot token.
	dg, err := discordgo.New("Bot " + s.Token)
	if err != nil {
		logger.Errorf("Creating Discord session: %s", err)
		lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Name, "guild": "None"}).Set(0)
		return
	}

	// get shards
	st, err := dg.GatewayBot()
	if err != nil {
		logger.Errorf("Creating Discord gateway: %s", err)
		lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Name, "guild": "None"}).Set(0)
		return
	}

	// shard into sessions.
	shards := make([]*discordgo.Session, st.Shards)
	for i := 0; i < st.Shards; i++ {
		shards[i], err = discordgo.New("Bot " + s.Token)
		if err != nil {
			logger.Errorf("Creating Discord sharded session: %s", err)
			lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Name, "guild": "None"}).Set(0)
			return
		}
		shards[i].ShardID = i
		shards[i].ShardCount = st.Shards
	}

	// open ws connections
	var errOpen error
	{
		wg := sync.WaitGroup{}
		for _, sess := range shards {
			wg.Add(1)
			go func(sess *discordgo.Session) {
				if err := sess.Open(); err != nil {
					errOpen = err
				}
				wg.Done()
			}(sess)
		}
		wg.Wait()
	}
	if errOpen != nil {
		wg := sync.WaitGroup{}
		for _, sess := range shards {
			wg.Add(1)
			go func(sess *discordgo.Session) {
				_ = sess.Close()
				wg.Done()
			}(sess)
		}
		wg.Wait()
	}

	// Get guides for bot
	guilds, err := dg.UserGuilds(100, "", "")
	if err != nil {
		logger.Errorf("Getting guilds: %s", err)
		s.Nickname = false
	}

	// check for frequency override
	if *frequency != 0 {
		s.Frequency = *frequency
	}

	// If other currency, get rate
	if s.Currency != "USD" {
		logger.Infof("Using %s", s.Currency)
		exData, err := utils.GetStockPrice(s.Currency + "=X")
		if err != nil {
			logger.Errorf("Unable to fetch exchange rate for %s, default to USD.", s.Currency)
		} else {
			if len(exData.QuoteSummary.Results) > 0 {
				s.Exrate = exData.QuoteSummary.Results[0].Price.RegularMarketPrice.Raw * float64(s.Multiplier)
			} else {
				logger.Errorf("Bad exchange rate for %s, default to USD.", s.Currency)
				s.Exrate = float64(s.Multiplier)
			}
		}
	} else {
		s.Exrate = float64(s.Multiplier)
	}

	// Set arrows if no custom decorator
	var arrows bool
	if s.Decorator == "" {
		arrows = true
	}

	// Grab custom activity messages
	var custom_activity []string
	itr := 0
	itrSeed := 0.0
	if s.Activity != "" {
		custom_activity = strings.Split(s.Activity, ";")
		if s.Multiplier != 1 {
			custom_activity = append(custom_activity, fmt.Sprintf("x%d %s", s.Multiplier, s.Name))
		}
	} else if s.Multiplier > 1 {
		custom_activity = append(custom_activity, fmt.Sprintf("x%d %s", s.Multiplier, strings.ToUpper(s.Name)))
	}

	// perform management operations
	if *managed {
		setName(dg, s.label())
	}

	logger.Infof("Watching crypto price for %s", s.Name)
	ticker := time.NewTicker(time.Duration(s.Frequency) * time.Second)

	// continuously watch
	for {
		select {
		case <-s.Close:
			logger.Infof("Shutting down price watching for %s", s.Name)
			return
		case <-ticker.C:
			logger.Debugf("Fetching crypto price for %s", s.Name)

			var priceData utils.GeckoPriceResults
			var fmtPrice string
			var fmtChange string
			var fmtDiffPercent string

			// get the coin price data
			if rdb == nilCache {
				priceData, err = utils.GetCryptoPrice(s.Name)
			} else {
				priceData, err = utils.GetCryptoPriceCache(rdb, ctx, s.Name)
				if err != nil {
					cacheMisses.Inc()
				} else {
					cacheHits.Inc()
				}
			}
			if err != nil {
				logger.Errorf("Unable to fetch crypto price for %s: %s", s.Name, err)
				if strings.Contains(err.Error(), "rate limited") {
					rateLimited.Inc()
				} else {
					updateError.Inc()
				}
				continue
			}

			// Check if conversion is needed
			if s.Exrate > 1.0 {
				priceData.MarketData.CurrentPrice.USD = s.Exrate * priceData.MarketData.CurrentPrice.USD
				priceData.MarketData.PriceChangeCurrency.USD = s.Exrate * priceData.MarketData.PriceChangeCurrency.USD
			}

			// format the price changes
			fmtDiffPercent = fmt.Sprintf("%.2f", priceData.MarketData.PriceChangePercent)
			fmtChange = fmt.Sprintf("%.2f", priceData.MarketData.PriceChangeCurrency.USD)

			// Check for custom decimal places
			if s.Decimals >= 0 && s.Decimals <= 13 {
				fmtTemplate := fmt.Sprintf("%%.%df", s.Decimals)
				fmtPrice = fmt.Sprintf(fmtTemplate, priceData.MarketData.CurrentPrice.USD)
			} else {
				// Check for cryptos below 1c
				if priceData.MarketData.CurrentPrice.USD < 0.01 {
					priceData.MarketData.CurrentPrice.USD = priceData.MarketData.CurrentPrice.USD * 100
					if priceData.MarketData.CurrentPrice.USD < 0.00001 {
						fmtPrice = fmt.Sprintf("%.8f¢", priceData.MarketData.CurrentPrice.USD)
					} else {
						fmtPrice = fmt.Sprintf("%.6f¢", priceData.MarketData.CurrentPrice.USD)
					}
				} else if priceData.MarketData.CurrentPrice.USD < 1.0 {
					fmtPrice = fmt.Sprintf("%.3f", priceData.MarketData.CurrentPrice.USD)
				} else {
					fmtPrice = fmt.Sprintf("%.2f", priceData.MarketData.CurrentPrice.USD)
				}
			}

			// calculate if price has moved up or down
			var increase bool
			if len(fmtChange) == 0 {
				increase = true
			} else if string(fmtChange[0]) == "-" {
				increase = false
			} else {
				increase = true
			}

			// add plus sign to diff percentage
			if priceData.MarketData.PriceChangePercent >= 0.0005 {
				fmtDiffPercent = "+" + fmtDiffPercent
			}

			// set arrows based on movement
			if arrows {
				s.Decorator = "⬊"
				if increase {
					s.Decorator = "⬈"
				}
			}

			// update nickname instead of activity
			if s.Nickname {
				var displayName string
				var nickname string
				var activity string

				// override coin symbol
				if s.Ticker != "" {
					displayName = s.Ticker
				} else {
					displayName = strings.ToUpper(priceData.Symbol)
				}

				// format nickname, looks like "0.059741 USD"
				nickname = fmt.Sprintf("%s %s", fmtPrice, strings.ToUpper(s.Currency))

				// format activity
				if s.Pair != "" {

					// get price of target pair
					var pairPriceData utils.GeckoPriceResults
					if rdb == nilCache {
						pairPriceData, err = utils.GetCryptoPrice(s.Pair)
					} else {
						pairPriceData, err = utils.GetCryptoPriceCache(rdb, ctx, s.Pair)
					}
					if err != nil {
						logger.Errorf("Unable to fetch pair price for %s: %s", s.Pair, err)
						activity = fmt.Sprintf("%s%%", fmtDiffPercent)
					} else {

						// set pair
						var pairPrice float64
						var pairSymbol string
						if s.PairFlip {
							pairPrice = pairPriceData.MarketData.CurrentPrice.USD / priceData.MarketData.CurrentPrice.USD
							pairSymbol = fmt.Sprintf("%s/%s", strings.ToUpper(pairPriceData.Symbol), displayName)
						} else {
							pairPrice = priceData.MarketData.CurrentPrice.USD / pairPriceData.MarketData.CurrentPrice.USD
							pairSymbol = fmt.Sprintf("%s/%s", displayName, strings.ToUpper(pairPriceData.Symbol))
						}

						// format decimals
						if pairPrice < 0.1 {
							activity = fmt.Sprintf("%.4f %s", pairPrice, pairSymbol)
						} else {
							activity = fmt.Sprintf("%.2f %s", pairPrice, pairSymbol)
						}
					}
				} else {
					// activity will be formatted like "-0.82 % | DOGEUSD"
					if math.Abs(priceData.MarketData.PriceChangeCurrency.USD) < 0.01 {
						activity = fmt.Sprintf("%s %% | %s%s", fmtDiffPercent, strings.ToUpper(priceData.Symbol), strings.ToUpper(s.Currency))
					} else {
						activity = fmt.Sprintf("%s %% | %s%s", fmtDiffPercent, strings.ToUpper(priceData.Symbol), strings.ToUpper(s.Currency))
					}
				}

				// Update nickname in guilds
				for _, g := range guilds {
					err = dg.GuildMemberNickname(g.ID, "@me", nickname)
					if err != nil {
						logger.Errorf("Updating nickname: %s", err)
						continue
					}
					logger.Debugf("Set nickname in %s: %s", g.Name, nickname)
					lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Name, "guild": g.Name}).SetToCurrentTime()

					if s.Color {
						// change bot color
						err = setRole(dg, s.ClientID, g.ID, increase)
						if err != nil {
							logger.Errorf("Color roles: %s", err)
						}
					}
				}

				// Custom activity messages
				if len(custom_activity) > 0 {

					// Display the real activity once per cycle
					if itr == len(custom_activity) {
						itr = 0
						itrSeed = 0.0
					} else if math.Mod(itrSeed, 2.0) == 1.0 {
						activity = custom_activity[itr]
						itr++
						itrSeed++
					} else {
						activity = custom_activity[itr]
						itrSeed++
					}
				}

				// set activity
				wg := sync.WaitGroup{}
				for _, sess := range shards {
					err = sess.UpdateStatusComplex(*newUpdateStatusData(0, discordgo.ActivityTypeWatching, activity, ""))

					if err != nil {
						logger.Errorf("Unable to set activity: %s", err)
					} else {
						logger.Debugf("Set activity: %s", activity)
						lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Name, "guild": "None"}).SetToCurrentTime()
					}
				}
				wg.Wait()

				time.Sleep(time.Duration(s.Frequency) * time.Second)
			} else {

				// format activity
				activity := fmt.Sprintf("%s %s %s%%", fmtPrice, s.Decorator, fmtDiffPercent)

				wg := sync.WaitGroup{}
				for _, sess := range shards {
					err = sess.UpdateGameStatus(0, activity)
					if err != nil {
						logger.Errorf("Unable to set activity: %s", err)
					} else {
						logger.Debugf("Set activity: %s", activity)
						lastUpdate.With(prometheus.Labels{"type": "ticker", "ticker": s.Name, "guild": "None"}).SetToCurrentTime()
					}
				}
				wg.Wait()
			}
		}
	}
}

func newUpdateStatusData(idle int, activityType discordgo.ActivityType, name, url string) *discordgo.UpdateStatusData {
	usd := &discordgo.UpdateStatusData{
		Status: "online",
	}

	if idle > 0 {
		usd.IdleSince = &idle
	}

	if name != "" {
		usd.Activities = []*discordgo.Activity{{
			Name: name,
			Type: activityType,
			URL:  url,
		}}
	}

	return usd
}
