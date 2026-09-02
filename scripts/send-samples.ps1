# Posts one sample enquiry per interesting pipeline path, signed the way a real
# provider would sign it. Usage:
#   $env:WEBHOOK_SECRET='...'; ./scripts/send-samples.ps1
[CmdletBinding()]
param([string]$BaseUrl = "http://localhost:8080")

$ErrorActionPreference = "Stop"
if (-not $env:WEBHOOK_SECRET) {
  throw "Set `$env:WEBHOOK_SECRET to the same value the API was started with."
}

function Send-Enquiry {
  param([string]$Channel, [string]$Body, [string]$Label)

  $hmac = [System.Security.Cryptography.HMACSHA256]::new(
    [Text.Encoding]::UTF8.GetBytes($env:WEBHOOK_SECRET))
  $hash = $hmac.ComputeHash([Text.Encoding]::UTF8.GetBytes($Body))
  $sig = ([BitConverter]::ToString($hash) -replace '-', '').ToLower()

  Write-Host "`n--- $Label ($Channel)"
  try {
    Invoke-RestMethod -Method Post -Uri "$BaseUrl/webhook/$Channel" `
      -ContentType 'application/json' `
      -Headers @{ 'X-Beda-Signature' = "sha256=$sig" } `
      -Body ([Text.Encoding]::UTF8.GetBytes($Body)) | ConvertTo-Json -Compress
  } catch {
    Write-Host "failed: $($_.Exception.Message)" -ForegroundColor Red
  }
}

# High-urgency sales: routes to enterprise-sales via the priority-1 rule.
Send-Enquiry email @'
{"from":"Jane Doe <jane.doe@acme-widgets.example>","subject":"Urgent: pricing for 500 units before Friday","body":"Hi,\n\nI am Jane Doe, procurement lead at Acme Widgets Ltd. We need 500 units of the WX-200 and a quote before Friday, our board signs off next week. My direct line is 020 7946 0958.\n\nThanks,\nJane"}
'@ "high-urgency sales"

# Support ticket from a web form: routes to support-l1.
Send-Enquiry web_form @'
{"name":"Sam Patel","email":"sam.patel@bigco.example","phone":"020 7946 1234","company":"BigCo Retail","message":"Our checkout integration started returning 502 errors this morning and orders are not going through. Account BC-4471. This is blocking live sales."}
'@ "urgent support"

# Missing required info: drafts a clarifying question instead of guessing.
Send-Enquiry messaging @'
{"sender_handle":"@curious_buyer","text":"hey do you do bulk discounts?"}
'@ "insufficient info"

# Marketing blast: caught by heuristics, confirmed by the model, archived as junk.
Send-Enquiry email @'
{"from":"growth@seo-deals.example","subject":"Boost your ranking with premium backlinks","body":"We offer premium SEO services and backlink packages. Guest post opportunities available now. https://a.example https://b.example https://c.example Click here to claim your discount."}
'@ "junk"

# Exact duplicate of the first enquiry: discarded before any LLM call.
Send-Enquiry email @'
{"from":"Jane Doe <jane.doe@acme-widgets.example>","subject":"Urgent: pricing for 500 units before Friday","body":"Hi,\n\nI am Jane Doe, procurement lead at Acme Widgets Ltd. We need 500 units of the WX-200 and a quote before Friday, our board signs off next week. My direct line is 020 7946 0958.\n\nThanks,\nJane"}
'@ "duplicate of the first enquiry"

# Same person, new enquiry: matches the existing contact on email and attaches.
Send-Enquiry email @'
{"from":"jane.doe@acme-widgets.example","subject":"Follow-up: lead times","body":"One more thing on the 500 unit order for Acme Widgets Ltd - what are your lead times if we sign this month? Jane Doe"}
'@ "returning contact"

Write-Host "`nOpen http://localhost:3000 to review the queue."
