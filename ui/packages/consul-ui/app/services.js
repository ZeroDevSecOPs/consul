export default {
  "route:basic": {
    "class": "consul-ui/routing/route"
  },
  "service:intl": {
    "class": "consul-ui/services/i18n"
  },
  "service:state": {
    "class": "consul-ui/services/state-with-charts"
  },
  "auth-provider:oidc-with-url": {
    "class": "consul-ui/services/auth-providers/oauth2-code-with-url-provider"
  }
}
