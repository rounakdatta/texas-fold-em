package whereabouts

import (
	"strings"
	"sync"
	"time"
)

// singleZone: currencies spent in one time zone. (The few exceptions — a
// Pacific island on New Zealand's dollar — are rarer than being wrong about
// them would matter.) The rest — dollars, euros, Australian and Canadian
// dollars, rupiah, pesos, reais, roubles — span zones, and a trip in them is
// placed by the cities it names.
var singleZone = map[string]string{
	"SGD": "Asia/Singapore", "AED": "Asia/Dubai", "GBP": "Europe/London", "JPY": "Asia/Tokyo",
	"THB": "Asia/Bangkok", "MYR": "Asia/Kuala_Lumpur", "HKD": "Asia/Hong_Kong", "MOP": "Asia/Macau",
	"KRW": "Asia/Seoul", "CNY": "Asia/Shanghai", "TWD": "Asia/Taipei", "PHP": "Asia/Manila",
	"VND": "Asia/Ho_Chi_Minh", "KHR": "Asia/Phnom_Penh", "LAK": "Asia/Vientiane", "MMK": "Asia/Yangon",
	"LKR": "Asia/Colombo", "NPR": "Asia/Kathmandu", "BTN": "Asia/Thimphu", "BDT": "Asia/Dhaka",
	"MVR": "Indian/Maldives", "MUR": "Indian/Mauritius", "SCR": "Indian/Mahe",
	"QAR": "Asia/Qatar", "SAR": "Asia/Riyadh", "OMR": "Asia/Muscat", "BHD": "Asia/Bahrain", "KWD": "Asia/Kuwait",
	"JOD": "Asia/Amman", "ILS": "Asia/Jerusalem", "TRY": "Europe/Istanbul", "EGP": "Africa/Cairo",
	"MAD": "Africa/Casablanca", "KES": "Africa/Nairobi", "TZS": "Africa/Dar_es_Salaam", "ZAR": "Africa/Johannesburg",
	"CHF": "Europe/Zurich", "SEK": "Europe/Stockholm", "NOK": "Europe/Oslo", "DKK": "Europe/Copenhagen",
	"ISK": "Atlantic/Reykjavik", "PLN": "Europe/Warsaw", "CZK": "Europe/Prague", "HUF": "Europe/Budapest",
	"RON": "Europe/Bucharest", "BGN": "Europe/Sofia", "RSD": "Europe/Belgrade", "GEL": "Asia/Tbilisi",
	"AMD": "Asia/Yerevan", "AZN": "Asia/Baku", "UZS": "Asia/Tashkent", "NZD": "Pacific/Auckland",
	"FJD": "Pacific/Fiji", "ARS": "America/Argentina/Buenos_Aires", "COP": "America/Bogota",
	"PEN": "America/Lima", "CLP": "America/Santiago", "CRC": "America/Costa_Rica",
}

// city is a city a trip's payees may name, with its time zone.
type city struct{ name, zone string }

// cityTable: per currency, cities by their zone — for the currencies that
// span zones, what places a trip; for the others, only what to call it. Only
// the unambiguous: a Springfield or a Richmond is left to a
// Resolver, which sees the trip around it — and no bare state name: the
// "Washington" of "Seattle, Washington" is not the capital. (A city is
// listed under a currency, so Paris in dollars — Texas's — is never Paris
// in euros.)
var cityTable = map[string]map[string][]string{
	"USD": {
		"America/New_York": {"New York|New York City|NYC|Manhattan|Brooklyn|Queens|Bronx|The Bronx|Staten Island|Harlem|Long Island City|Jersey City|Hoboken",
			"Newark", "Boston", "Providence", "Hartford", "New Haven", "Philadelphia", "Pittsburgh", "Baltimore",
			"Washington DC|Washington D.C.|DC", "Charlotte", "Raleigh", "Atlanta", "Savannah", "Orlando",
			"Miami|Miami Beach", "Tampa", "Fort Lauderdale", "Jacksonville", "Buffalo", "Niagara Falls", "Cleveland",
			"Columbus", "Cincinnati"},
		"America/Detroit":              {"Detroit"},
		"America/Indiana/Indianapolis": {"Indianapolis"},
		"America/Kentucky/Louisville":  {"Louisville"},
		"America/Chicago": {"Chicago", "Houston", "Dallas", "Austin", "San Antonio", "New Orleans", "Nashville", "Memphis",
			"Minneapolis", "St. Louis|St Louis|Saint Louis", "Kansas City", "Milwaukee", "Oklahoma City", "Omaha"},
		"America/Denver":      {"Denver", "Salt Lake City", "Albuquerque", "Santa Fe"},
		"America/Boise":       {"Boise"},
		"America/Phoenix":     {"Phoenix", "Tucson", "Scottsdale", "Sedona"},
		"America/Los_Angeles": {"Los Angeles", "San Francisco|SF|San Fran", "Oakland", "Berkeley", "San Jose", "Palo Alto", "Mountain View", "Sunnyvale", "Santa Clara", "Cupertino", "Menlo Park", "Redwood City", "San Mateo", "Sausalito", "San Diego", "Santa Monica", "Pasadena", "Anaheim", "Irvine", "Sacramento", "Napa", "Seattle", "Tacoma", "Redmond", "Las Vegas", "Reno"},
		"America/Anchorage":   {"Anchorage"},
		"Pacific/Honolulu":    {"Honolulu", "Waikiki", "Maui"},
		"Asia/Phnom_Penh":     {"Phnom Penh", "Siem Reap"},
		"America/Guayaquil":   {"Quito", "Guayaquil"},
		"America/El_Salvador": {"San Salvador"},
	},
	"EUR": {
		"Europe/Paris":      {"Paris", "Lyon", "Marseille", "Nice", "Bordeaux", "Toulouse", "Strasbourg"},
		"Europe/Berlin":     {"Berlin", "Munich|München|Muenchen", "Frankfurt", "Hamburg", "Cologne|Köln", "Düsseldorf|Dusseldorf", "Stuttgart", "Dresden", "Heidelberg"},
		"Europe/Amsterdam":  {"Amsterdam", "Rotterdam", "The Hague|Den Haag", "Utrecht"},
		"Europe/Brussels":   {"Brussels|Bruxelles", "Bruges|Brugge", "Antwerp", "Ghent"},
		"Europe/Rome":       {"Rome|Roma", "Milan|Milano", "Venice|Venezia", "Florence|Firenze", "Naples|Napoli", "Pisa", "Bologna", "Turin|Torino"},
		"Europe/Madrid":     {"Madrid", "Barcelona", "Seville|Sevilla", "Valencia", "Granada", "Malaga|Málaga", "Bilbao"},
		"Atlantic/Canary":   {"Tenerife", "Las Palmas", "Gran Canaria"},
		"Europe/Lisbon":     {"Lisbon|Lisboa", "Porto", "Faro"},
		"Atlantic/Madeira":  {"Funchal", "Madeira"},
		"Europe/Dublin":     {"Dublin", "Cork", "Galway"},
		"Europe/Vienna":     {"Vienna|Wien", "Salzburg", "Innsbruck"},
		"Europe/Athens":     {"Athens", "Thessaloniki", "Santorini", "Mykonos", "Heraklion"},
		"Europe/Helsinki":   {"Helsinki"},
		"Europe/Tallinn":    {"Tallinn"},
		"Europe/Riga":       {"Riga"},
		"Europe/Vilnius":    {"Vilnius"},
		"Europe/Luxembourg": {"Luxembourg"},
		"Europe/Bratislava": {"Bratislava"},
		"Europe/Ljubljana":  {"Ljubljana"},
		"Europe/Zagreb":     {"Zagreb", "Split", "Dubrovnik"},
		"Europe/Malta":      {"Valletta"},
		"Asia/Nicosia":      {"Nicosia", "Limassol"},
		"Europe/Monaco":     {"Monaco", "Monte Carlo"},
	},
	"AUD": {
		"Australia/Sydney":    {"Sydney", "Canberra"},
		"Australia/Melbourne": {"Melbourne"},
		"Australia/Brisbane":  {"Brisbane", "Gold Coast", "Cairns"},
		"Australia/Perth":     {"Perth"},
		"Australia/Adelaide":  {"Adelaide"},
		"Australia/Hobart":    {"Hobart"},
		"Australia/Darwin":    {"Darwin"},
	},
	"CAD": {
		"America/Toronto":   {"Toronto", "Ottawa", "Montreal|Montréal", "Quebec City|Québec", "Niagara Falls"},
		"America/Vancouver": {"Vancouver", "Victoria", "Whistler"},
		"America/Edmonton":  {"Calgary", "Edmonton", "Banff"},
		"America/Winnipeg":  {"Winnipeg"},
		"America/Halifax":   {"Halifax"},
	},
	"IDR": {
		"Asia/Jakarta":  {"Jakarta", "Bandung", "Yogyakarta"},
		"Asia/Makassar": {"Bali", "Denpasar", "Ubud", "Kuta", "Seminyak", "Canggu", "Lombok"},
	},
	"MXN": {
		"America/Mexico_City": {"Mexico City|CDMX", "Guadalajara", "Monterrey", "Oaxaca"},
		"America/Cancun":      {"Cancun|Cancún", "Tulum", "Playa del Carmen"},
		"America/Tijuana":     {"Tijuana"},
	},
	"BRL": {"America/Sao_Paulo": {"São Paulo|Sao Paulo", "Rio de Janeiro|Rio"}},
	"RUB": {"Europe/Moscow": {"Moscow", "St Petersburg|Saint Petersburg"}},
	// single-zone currencies: only what to call the zone, by the city named
	"SGD": {"Asia/Singapore": {"Singapore"}},
	"AED": {"Asia/Dubai": {"Dubai", "Abu Dhabi", "Sharjah"}},
	"GBP": {"Europe/London": {"London", "Edinburgh", "Manchester", "Liverpool", "Oxford", "Glasgow", "Bath"}},
	"JPY": {"Asia/Tokyo": {"Tokyo", "Kyoto", "Osaka", "Nara", "Hiroshima", "Sapporo"}},
	"THB": {"Asia/Bangkok": {"Bangkok", "Phuket", "Chiang Mai", "Krabi", "Pattaya"}},
	"MYR": {"Asia/Kuala_Lumpur": {"Kuala Lumpur|KL", "Penang", "Langkawi", "Malacca|Melaka"}},
	"CHF": {"Europe/Zurich": {"Zurich|Zürich", "Geneva", "Lucerne", "Interlaken", "Zermatt", "Bern"}},
}

var cityIndex struct {
	once  sync.Once
	byCur map[string]map[string]city // currency → normalized name → city
	names map[string]string          // zone → the first city listed for it
}

func loadCities() {
	cityIndex.byCur, cityIndex.names = map[string]map[string]city{}, map[string]string{}
	for cur, zones := range cityTable {
		m := map[string]city{}
		for zone, entries := range zones {
			for _, e := range entries {
				names := strings.Split(e, "|")
				for _, n := range names {
					m[normalize(n)] = city{name: names[0], zone: zone}
				}
				if _, ok := cityIndex.names[zone]; !ok {
					cityIndex.names[zone] = names[0]
				}
			}
		}
		cityIndex.byCur[cur] = m
	}
}

// cityIn is a city by its name, among those the currency is spent in.
func cityIn(cur, name string) (city, bool) {
	cityIndex.once.Do(loadCities)
	c, ok := cityIndex.byCur[cur][normalize(name)]
	return c, ok
}

// currencyZone is a single-zone currency's zone.
func currencyZone(cur string) (*time.Location, bool) {
	z, ok := singleZone[cur]
	if !ok {
		return nil, false
	}
	loc, err := time.LoadLocation(z)
	return loc, err == nil
}

// zoneName is what to call a zone the trip names no city in: the first city
// listed for it ("Los Angeles"), else its own name ("Kuala Lumpur").
func zoneName(zone string) string {
	cityIndex.once.Do(loadCities)
	if n, ok := cityIndex.names[zone]; ok {
		return n
	}
	i := strings.LastIndex(zone, "/")
	return strings.ReplaceAll(zone[i+1:], "_", " ")
}
