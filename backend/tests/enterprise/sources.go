package enterprise

import (
	"fmt"
	"time"
)

type object = map[string]any

var Sources = []string{"erp", "salesforce", "shopify", "braze", "emarsys", "loyalty", "identity"}
var launch = map[string]int{"erp": 2011, "salesforce": 2014, "emarsys": 2015, "shopify": 2017, "loyalty": 2018, "braze": 2020, "identity": 2022}

type person struct {
	n                                                  int
	first, last, email, phone, country, language, city string
}

func personAt(i int) person {
	countries := []string{"AT", "DE", "FR", "NL", "ES", "IT", "PL", "GB"}
	languages := []string{"de", "de", "fr", "nl", "es", "it", "pl", "en"}
	cities := []string{"Wien", "Berlin", "Lyon", "Utrecht", "Valencia", "Torino", "Kraków", "London"}
	first := []string{"Mara", "Lukas", "Élodie", "Noor", "Lucía", "Giulia", "Łukasz", "Alex"}
	last := []string{"Muster", "Beispiel", "Exemple", "Voorbeeld", "Ejemplo", "Esempio", "Przykład", "Example"}
	// UK Ofcom drama range. Reuse is deliberate; phone is never a unique identity key.
	return person{i, first[i%8], last[i%8], fmt.Sprintf("person-%06d@example.com", i), fmt.Sprintf("+44 1632 960%03d", i%1000), countries[i%8], languages[i%8], cities[i%8]}
}
func sourceSet(cohort string, i int, c Config) []string {
	switch cohort {
	case "crm_only":
		return []string{"salesforce"}
	case "marketing_only":
		if c.draw(i, "marketing") < .5 {
			return []string{"braze"}
		}
		return []string{"emarsys"}
	case "ecommerce_only":
		return []string{"shopify"}
	case "dormant_erp":
		return []string{"erp"}
	case "fragmented":
		return []string{"shopify", "braze", "emarsys"}
	default:
		out := []string{"erp", "salesforce", "shopify"}
		for _, s := range []string{"braze", "emarsys", "loyalty", "identity"} {
			if c.draw(i, "coverage/"+s) < .7 {
				out = append(out, s)
			}
		}
		return out
	}
}

// 15-character Salesforce IDs (case-sensitive form), not invented 18-character checksums.
func sfid(prefix string, n int) string { return fmt.Sprintf("%s%012d", prefix, n) }
func recordID(source string, n, duplicate int) string {
	k := n*10 + duplicate
	switch source {
	case "salesforce":
		if n%5 == 0 {
			return sfid("00Q", k)
		}
		return sfid("003", k)
	case "shopify":
		return fmt.Sprintf("gid://shopify/Customer/%d", 900000000+k)
	case "braze":
		return fmt.Sprintf("%024x", k+1)
	default:
		return fmt.Sprintf("%s-%08d", source, k)
	}
}
func rawRecord(source, id string, p person, flags []string, created, updated time.Time) object {
	has := func(k string) bool { return contains(flags, k) }
	email := p.email
	if has("multiple_emails") && (source == "shopify" || source == "braze") {
		email = fmt.Sprintf("purchase-%06d@example.com", p.n)
	}
	if has("shared_email") {
		email = personAt(p.n - p.n%2).email
	}
	last := p.last
	if has("name_change") && source != "erp" {
		last += "-Renamed"
	}
	phone := p.phone
	if has("phone_change") && source != "erp" {
		phone = fmt.Sprintf("01632 960%03d", (p.n+17)%1000)
	}
	if has("conflicting_values") && source == "salesforce" {
		last = "Conflicting-" + last
	}
	stamp := func(t time.Time) string { return t.Format(time.RFC3339) }
	consent := "opted_in"
	if has("consent_lag") && source == "shopify" {
		consent = "unsubscribed"
	}
	switch source {
	case "salesforce":
		if len(id) >= 3 && id[:3] == "00Q" {
			return object{"attributes": object{"type": "Lead", "url": "/services/data/v65.0/sobjects/Lead/" + id}, "Id": id, "FirstName": p.first, "LastName": last, "Email": email, "Phone": phone, "Company": "Synthetic retail household", "Status": "Open - Not Contacted", "IsConverted": false, "IsDeleted": has("deleted_downstream"), "OwnerId": sfid("005", 1+p.n%3), "CreatedDate": stamp(created), "LastModifiedDate": stamp(updated)}
		}
		return object{"attributes": object{"type": "Contact", "url": "/services/data/v65.0/sobjects/Contact/" + id}, "Id": id, "FirstName": p.first, "LastName": last, "Email": email, "Phone": phone, "MailingCountry": p.country, "AccountId": sfid("001", p.n/4+1), "OwnerId": sfid("005", 1+p.n%3), "CreatedDate": stamp(created), "LastModifiedDate": stamp(updated), "IsDeleted": has("deleted_downstream")}
	case "braze":
		user := object{"braze_id": id, "external_id": fmt.Sprintf("retail-%08d", p.n), "first_name": p.first, "last_name": last, "email": email, "language": p.language, "country": p.country, "phone": phone, "email_subscribe": consent, "created_at": stamp(created), "user_aliases": []object{{"alias_name": fmt.Sprintf("web-%d", p.n), "alias_label": "synthetic_web"}}, "custom_attributes": object{"crm_contact_id": sfid("003", p.n*10), "loyalty_points": p.n % 1200}, "purchases": []object{{"name": "synthetic-scarf", "first": stamp(updated), "last": stamp(updated), "count": 1}}}
		if has("missing_reference") {
			user["custom_attributes"].(object)["crm_contact_id"] = sfid("003", 999999999)
		}
		if has("missing_external_id") || has("anonymous") {
			delete(user, "external_id")
			delete(user, "email")
			delete(user, "first_name")
			delete(user, "last_name")
		}
		return object{"message": "success", "users": []object{user}}
	case "shopify":
		marketing := "SUBSCRIBED"
		if consent == "unsubscribed" {
			marketing = "UNSUBSCRIBED"
		}
		node := object{"id": id, "firstName": p.first, "lastName": last, "defaultEmailAddress": object{"emailAddress": email, "marketingState": marketing}, "defaultPhoneNumber": object{"phoneNumber": phone}, "createdAt": stamp(created), "updatedAt": stamp(updated), "numberOfOrders": fmt.Sprint(p.n % 40), "state": "DISABLED", "amountSpent": object{"amount": fmt.Sprintf("%d.50", p.n%700), "currencyCode": "EUR"}, "defaultAddress": object{"address1": "Synthetic Test Street 0", "city": p.city, "zip": "0000", "countryCodeV2": p.country}, "tags": []string{"synthetic"}}
		if has("address_change") {
			node["defaultAddress"].(object)["address1"] = "Synthetic New Street 0"
		}
		if has("missing_external_id") {
			node["id"] = nil
		}
		return object{"data": object{"customers": object{"nodes": []object{node}, "pageInfo": object{"hasNextPage": false, "endCursor": nil}}}}
	case "emarsys":
		// Account-specific synthetic mapping; key_id points to our fictional indexed retail ID field.
		contact := object{"1": p.first, "2": last, "3": email, "9001": fmt.Sprintf("retail-%08d", p.n), "9002": "yes", "9003": stamp(updated)}
		if has("missing_external_id") {
			delete(contact, "9001")
		}
		if has("reused_identifier") {
			contact["9001"] = fmt.Sprintf("retail-%08d", p.n-p.n%2)
		}
		return object{"key_id": "9001", "contacts": []object{contact}}
	case "erp":
		country := p.country
		if has("schema_variation") {
			country = "UK"
		}
		date := created.Format("02.01.2006")
		if p.n%2 == 0 {
			date = created.Format("01/02/06")
		}
		row := object{"KUNDENNR": id, "VORNAME": p.first, "NACHNAME": last, "EMAIL": email, "TELEFON": phone, "LAND": country, "ORT": p.city, "PLZ": "", "STRASSE": "Testgasse 0", "ANGELEGT": date, "AKTIV": !has("inactive"), "FORMAT_VERSION": "2011"}
		if p.n%3 == 0 {
			row["EMAIL"] = ""
		}
		if has("schema_variation") {
			row["FORMAT_VERSION"] = "2019"
			row["created_on"] = stamp(created)
			delete(row, "ANGELEGT")
		}
		if has("malformed") {
			row["ANGELEGT"] = "31.02.2012"
			row["EMAIL"] = "not-an-email"
		}
		return row
	case "loyalty":
		idref := fmt.Sprintf("retail-%08d", p.n)
		if has("missing_external_id") {
			idref = ""
		}
		status := "active"
		if has("inactive") {
			status = "suspended"
		}
		if has("loyalty_delay") {
			idref = ""
		}
		return object{"membership_id": id, "customer_reference": idref, "enrolled_at": stamp(created), "status": status, "points": p.n % 7000, "email": email}
	default:
		// Internal identity enrollment export, deliberately not advertised as Kratos API output.
		return object{"enrollment_id": id, "retail_reference": fmt.Sprintf("retail-%08d", p.n), "verified_email": email, "verified_at": stamp(updated), "status": "active"}
	}
}

// Controlled wrong-type values exercise rejection without corrupting the JSONL framing.
func malform(source string, raw object) {
	switch source {
	case "salesforce":
		raw["LastName"] = []string{"malformed"}
	case "braze":
		raw["users"].([]object)[0]["last_name"] = []string{"malformed"}
	case "shopify":
		raw["data"].(object)["customers"].(object)["nodes"].([]object)[0]["lastName"] = []string{"malformed"}
	case "emarsys":
		raw["contacts"].([]object)[0]["2"] = []string{"malformed"}
	case "loyalty":
		raw["email"] = []string{"malformed"}
	}
}
