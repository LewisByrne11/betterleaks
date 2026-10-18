package rules

import (
	"github.com/betterleaks/betterleaks/v2/cmd/generate/config/utils"
	"github.com/betterleaks/betterleaks/v2/cmd/generate/secrets"
	"github.com/betterleaks/betterleaks/v2/config"
)

func OktaDomain() *config.Rule {
	r := config.Rule{
		ID:          "okta-domain",
		Confidence:  "high",
		Description: "Okta tenant domain, used as an optional component for validating Okta credentials.",
		Regex:       `(?i)(?:^|[^a-z0-9.-])([a-z0-9-]{1,40}\.(?:okta(?:preview|-emea|-gov)?\.com|okta\.mil))(?:[^a-z0-9.-]|$)`,
		Keywords: []string{
			".okta",
		},
		SkipReport: true,
		FilterExpr: `matchesAny(finding["secret"], [` + "`(?i)^(?:developer|www|help|support|status|login|forum|sec|example|invalid|subdomain|your-?domain)\\.`" + `])`,
	}

	tps := []string{
		`okta_domain = "dev-123456.okta.com"`,
		`orgUrl: "https://acme.oktapreview.com"`,
		`issuer = "https://tenant.okta-emea.com/oauth2/default"`,
		`domain = "agency.okta-gov.com"`,
		`domain = "dod.okta.mil"`,
	}
	fps := []string{
		`domain = "example.com"`,
		`domain = "okta.com.evil.example"`,
		`domain = "developer.okta.com"`,
		`domain = "www.okta.com"`,
		`domain = "example.okta.com"`,
		`domain = "invalid.okta.com"`,
		`domain = "subdomain.okta.com"`,
		`domain = "your-domain.okta.com"`,
	}
	return utils.Validate(r, tps, fps)
}

func OktaAccessToken() *config.Rule {
	// define rule
	r := config.Rule{
		ID:          "okta-access-token",
		Confidence:  "high",
		Description: "Identified an Okta Access Token, which may compromise identity management services and user authentication data.",
		Regex:       utils.GenerateSemiGenericRegex([]string{`(?-i:[Oo]kta|OKTA)`}, `00[\w=\-]{40}`, false),
		Keywords: []string{
			"okta",
		},
		Components: []config.Component{
			{RuleID: "okta-domain", Optional: true, Within: "20L"},
		},
		ValidateExpr: `let domain = components["okta-domain"]?.secret ?? "";
domain == "" ? {
  "result": "needs_validation",
  "reason": "Okta access token verification requires an Okta tenant domain"
} : (
  let r = http.get("https://" + domain + "/api/v1/users/me", {
    "Accept": "application/json",
    "Content-Type": "application/json",
    "Authorization": "SSWS " + finding["secret"]
  });
  r.status == 200 && (r.body contains "\"activated\":") ? {
    "result": "valid"
  } : r.status == 401 ? {
    "result": "invalid",
    "reason": "Unauthorized"
  } : validate.unknown(r)
)`,
		FilterExpr: `entropy(finding["secret"]) <= 4.0`,
	}

	// validate
	tps := utils.GenerateSampleSecrets("okta", secrets.NewSecretWithEntropy(`00[\w=\-]{40}`, 4))
	tps = append(tps,
		`"oktaApiToken": "00ebObu4zSNkyc6dimLvUwq4KpTEop-PCEnnfSTpD3",`,       // betterleaks:allow
		`			var OktaApiToken = "00fWkOjwwL9xiFd-Vfgm_ePATIRxVj852Iblbb1DS_";`, // betterleaks:allow
	)
	fps := []string{
		`oktaKey = 00000000000000000000000000000000000TUVWXYZ`,   // low entropy
		`rookTable = 0023452Lllk2KqjLBvaxANWEgTd7bqjsxjo8aZj0wd`, // wrong case
	}
	return utils.Validate(r, tps, fps)
}

func OktaClientID() *config.Rule {
	r := config.Rule{
		ID:          "okta-client-id",
		Confidence:  "medium",
		Description: "Identified an Okta OAuth 2.0 Client ID, used as a component of the Okta OAuth client-secret rule.",
		Regex:       `(?:[^a-zA-Z0-9_-]|^)(0oa[a-zA-Z0-9]{17})(?:[^a-zA-Z0-9_-]|$)`,
		Keywords: []string{
			"0oa",
		},
		SkipReport: true,
		FilterExpr: utils.MinEntropy(3.0),
	}

	tps := []string{
		`client_id = "0oakDa9U4UqGWlG6g3Ot"`,
	}
	fps := []string{
		`client_id = "0oa00000000000000000"`,     // low entropy
		`client_id = "0obkDa9U4UqGWlG6g3Ot"`,     // wrong prefix
		`client_id = "0oakDa9U4UqGWlG6g3Ot999"`,  // too long (partial match guard)
		`client_id = "foo-0oakDa9U4UqGWlG6g3Ot"`, // hyphen-prefixed partial match
	}
	return utils.Validate(r, tps, fps)
}

func OktaOAuthClientSecret() *config.Rule {
	// Delimiters consume a non-secret char instead of using \b, since \b breaks on secrets
	// starting or ending in '-' (a non-word char that is also part of the secret charset).
	r := config.Rule{
		ID:          "okta-oauth-client-secret",
		Confidence:  "high",
		Description: "Identified an Okta OAuth 2.0 Client Secret, which could allow unauthorized access to Okta applications and APIs.",
		Regex:       `(?i)(?:client[_-]?secret|(?-i:[Oo]kta|OKTA)[_.-]?(?:[a-z0-9_.-]+[_.-])?secret)(?:[ \t\w.-]{0,20})[\s'"]{0,3}(?:=|>|:{1,3}=|\|\||:|=>|\?=|,)[\x60'"\s=]{0,5}([a-zA-Z0-9_-]{40,64})(?:[^a-zA-Z0-9_-]|$)`,
		Keywords: []string{
			"secret",
		},
		Components: []config.Component{
			{RuleID: "okta-client-id", Within: "20L"},
			{RuleID: "okta-domain", Optional: true, Within: "20L"},
		},
		ValidateExpr: `let domain = components["okta-domain"]?.secret ?? "";
domain == "" ? {
  "result": "needs_validation",
  "reason": "Okta OAuth client secret verification requires an Okta tenant domain"
} : (
  let r = http.post("https://" + domain + "/oauth2/default/v1/token", {
    "Accept": "application/json",
    "Content-Type": "application/x-www-form-urlencoded",
    "Authorization": "Basic " + base64.encode(bytes((components["okta-client-id"]?.secret ?? "") + ":" + finding["secret"]))
  }, "grant_type=client_credentials");
  r.status == 200 && (r.json?.access_token ?? "") != "" ? {
    "result": "valid"
  } : r.status == 400 && (r.json?.error ?? "") == "unauthorized_client" ? {
    "result": "valid"
  } : r.status == 401 || (r.status == 400 && (r.json?.error ?? "") == "invalid_client") ? {
    "result": "invalid",
    "reason": "Invalid client credentials"
  } : validate.unknown(r)
)`,
		FilterExpr: `entropy(finding["secret"]) < 3.5 || tokenRatio(finding["secret"]) >= 2.5`,
	}

	tps := []string{
		"okta_client_id = '0oakDa9U4UqGWlG6g3Ot'\nclient_secret: 'OhbVrpoiVgRV5IfLBcbfnoGMbJmTPSIAoCLrZ3aW'",
		"client_id = \"0oakDa9U4UqGWlG6g3Ot\"\nclient_secret = \"wj9bOsLK0BRNJqy7KMMnaE7m8qrW51bPO2n1-PYvkOmhHRYgcuOecQkEwq9MPYa5\"",
		"client_id = \"0oakDa9U4UqGWlG6g3Ot\"\nclient_secret = \"-u8jzPde0IgxLd6GncfBAepfJBd0Kh8oOOL8dKLz\"",
		"client_id = \"0oakDa9U4UqGWlG6g3Ot\"\nclient_secret = \"u8jzPde0IgxLd6GncfBAepfJBd0Kh8oOOL8dKLz-\"",
		"client_id = \"0oakDa9U4UqGWlG6g3Ot\"\nclient_secret = \"-u8jzPde0IgxLd6GncfBAepfJBd0Kh8oOOL8dKLzd\"",
		"client_id = \"0oakDa9U4UqGWlG6g3Ot\"\nclient_secret = \"u8jzPde0IgxLd6GncfBAepfJBd0Kh8oOOL8dKLzd-\"",
		"okta.oauth2.client-id=0oakDa9U4UqGWlG6g3Ot\nokta.oauth2.client-secret=OhbVrpoiVgRV5IfLBcbfnoGMbJmTPSIAoCLrZ3aW",
		"clientId: \"0oakDa9U4UqGWlG6g3Ot\"\nclientSecret: \"OhbVrpoiVgRV5IfLBcbfnoGMbJmTPSIAoCLrZ3aW\"",
	}
	fps := []string{
		"client_id = '0oakDa9U4UqGWlG6g3Ot'\napi_key: 'OhbVrpoiVgRV5IfLBcbfnoGMbJmTPSIAoCLrZ3aW'",
		"client_id = '0oakDa9U4UqGWlG6g3Ot'\nclient_secret: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'",
		"client_id = '0oakDa9U4UqGWlG6g3Ot'\nclient_secret: 'write_only_secret_value_at_least_32_chars'",
		"client_id = '0oakDa9U4UqGWlG6g3Ot'\nclient_secret: 'short'",
	}
	return utils.Validate(r, tps, fps)
}
