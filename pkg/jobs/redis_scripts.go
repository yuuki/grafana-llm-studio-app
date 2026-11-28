package jobs

import "github.com/redis/go-redis/v9"

// --- Atomic Redis Lua scripts (100% declared KEYS[], zero dynamic key access, Redis TIME authoritative) ---

const luaCommonHelpers = `
local function resolve_now_ms(argNowMs)
	if argNowMs and argNowMs > 0 then
		return argNowMs
	end
	local t = redis.call('TIME')
	return (tonumber(t[1]) or 0) * 1000 + math.floor((tonumber(t[2]) or 0) / 1000)
end

local function hget_num(key, field)
	return tonumber(redis.call('HGET', key, field)) or 0
end

local function hget_str(key, field)
	local v = redis.call('HGET', key, field)
	if not v then return '' end
	return v
end

local function safe_pexpire(key, ms)
	if ms < 1 then ms = 1 end
	redis.call('PEXPIRE', key, ms)
end

-- Monotonically extends TTL on shared index/slot keys so shorter-TTL callers never truncate longer-lived keys.
local function extend_pexpire(key, ms)
	if ms < 1 then ms = 1 end
	local cur = redis.call('PTTL', key)
	if cur < ms then
		redis.call('PEXPIRE', key, ms)
	end
end

local function is_active(jobKey, nowMs)
	return hget_num(jobKey, 'active_op_until_ms') > nowMs
end

local function is_expired(jobKey, nowMs)
	if redis.call('EXISTS', jobKey) == 0 then
		return true
	end
	if is_active(jobKey, nowMs) then
		return false
	end
	local expMs = hget_num(jobKey, 'expires_at_ms')
	return expMs > 0 and nowMs > expMs
end

-- Metadata format in orgMeta[jobID]: "<ownerKey>|<expiresAtMs>|<activeOpUntilMs>"
local function encode_meta(oKey, expMs, activeUntilMs)
	return oKey .. '|' .. tostring(expMs) .. '|' .. tostring(activeUntilMs)
end

local function parse_meta(raw)
	if not raw or raw == '' then
		return nil, 0, 0
	end
	local oKey, expStr, actStr = string.match(raw, '^([^|]+)|([^|]+)|([^|]+)$')
	if not oKey then
		return nil, 0, 0
	end
	return oKey, tonumber(expStr) or 0, tonumber(actStr) or 0
end

local function meta_is_active(activeUntilMs, nowMs)
	return activeUntilMs > nowMs
end

local function meta_is_expired(expMs, activeUntilMs, nowMs)
	if meta_is_active(activeUntilMs, nowMs) then
		return false
	end
	return expMs > 0 and nowMs > expMs
end

-- Reaps expired entries using ONLY declared orgIdx, orgMeta, and (optionally) userIdx keys.
-- Returns array of "<jobID>|<ownerKey>" entries for Go-side job hash deletion and cross-user ZREM.
local function reap_org_meta(orgIdx, orgMeta, userIdx, callerOKey, nowMs)
	local evicted = {}
	local members = redis.call('ZRANGE', orgIdx, 0, -1)
	for _, id in ipairs(members) do
		local raw = redis.call('HGET', orgMeta, id)
		local oKey, expMs, actMs = parse_meta(raw)
		if (not oKey) or meta_is_expired(expMs, actMs, nowMs) then
			redis.call('ZREM', orgIdx, id)
			redis.call('HDEL', orgMeta, id)
			if userIdx and oKey and oKey == callerOKey then
				redis.call('ZREM', userIdx, id)
			end
			if oKey then
				table.insert(evicted, id .. '|' .. oKey)
			else
				table.insert(evicted, id)
			end
		end
	end
	-- Reconcile ghost entries in caller's userIdx that are no longer in orgMeta.
	if userIdx then
		local uMembers = redis.call('ZRANGE', userIdx, 0, -1)
		for _, id in ipairs(uMembers) do
			if redis.call('HEXISTS', orgMeta, id) == 0 then
				redis.call('ZREM', userIdx, id)
			end
		end
	end
	return evicted
end

local function read_snapshot(jobKey)
	return redis.call('HMGET', jobKey,
		'id',
		'datasource_uid',
		'query_kind',
		'scope_json',
		'import_source_json',
		'status',
		'current_ir_json',
		'current_revision',
		'hist_len',
		'last_diff_json',
		'summary',
		'warnings_json',
		'error_message',
		'repair_attempts_used',
		'max_repair_attempts',
		'llm_meta_json',
		'updated_at_ms',
		'job_kind',
		'report_json'
	)
end

local function merge_lines_bounded(existing, incoming, maxCount)
	local seen = {}
	local out = {}
	local function add(src)
		if not src or src == '' then return end
		for line in string.gmatch(src, '[^\r\n]+') do
			if line ~= '' and not seen[line] then
				if #out >= maxCount then
					return
				end
				seen[line] = true
				table.insert(out, line)
			end
		end
	end
	add(existing)
	add(incoming)
	table.sort(out)
	return table.concat(out, '\n')
end

local function contains_line(haystack, needle)
	if not haystack or haystack == '' or not needle or needle == '' then
		return false
	end
	if string.find(needle, '\n', 1, true) or string.find(needle, '\r', 1, true) then
		return false
	end
	return string.find('\n' .. haystack .. '\n', '\n' .. needle .. '\n', 1, true) ~= nil
end

-- Verifies job existence, expiration (only owner may delete expired hash), exact (org_id, user_login) match,
-- and non-evicted membership in orgMeta. Returns nil on success, or an ERR reply table.
local function check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
	if redis.call('EXISTS', jobKey) == 0 then
		return {'ERR', 'NOT_FOUND'}
	end
	local storedOrg = hget_str(jobKey, 'org_id')
	local storedUser = hget_str(jobKey, 'user_login')
	local isOwner = (storedOrg == orgID and storedUser == userLogin)
	if is_expired(jobKey, nowMs) then
		if isOwner then
			redis.call('DEL', jobKey)
			redis.call('ZREM', orgIdx, jobID)
			redis.call('ZREM', userIdx, jobID)
			redis.call('HDEL', orgMeta, jobID)
		end
		return {'ERR', 'NOT_FOUND'}
	end
	if not isOwner then
		return {'ERR', 'FORBIDDEN'}
	end
	if redis.call('HEXISTS', orgMeta, jobID) == 0 then
		redis.call('DEL', jobKey)
		redis.call('ZREM', orgIdx, jobID)
		redis.call('ZREM', userIdx, jobID)
		return {'ERR', 'NOT_FOUND'}
	end
	return nil
end
`

var redisScriptCreateGenerating = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]
local orgSlots = KEYS[5]
local userSlots = KEYS[6]
local orgsSet = KEYS[7]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local dsUID = ARGV[5]
local qKind = ARGV[6]
local scopeJSON = ARGV[7]
local maxRepairs = ARGV[8]
local knownGauges = ARGV[9]
local slotID = ARGV[10]
local nowMs = resolve_now_ms(tonumber(ARGV[11]) or 0)
local jobTTLMs = tonumber(ARGV[12]) or 1
local leaseTTLMs = tonumber(ARGV[13]) or 1
local maxJobs = tonumber(ARGV[14]) or 1
local maxJobsPerUser = tonumber(ARGV[15]) or 1
local maxConcurrent = tonumber(ARGV[16]) or 1
local maxConcurrentPerUser = tonumber(ARGV[17]) or 1
local idxBufMs = tonumber(ARGV[18]) or 60000
local maxGauges = tonumber(ARGV[19]) or 512
local jobKind = ARGV[20] or ''
local initStatus = ARGV[21]
if not initStatus or initStatus == '' then
	initStatus = 'generating'
end

-- 1. Reap expired concurrency slot leases and check per-org & per-user limits.
redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
if redis.call('ZCARD', orgSlots) >= maxConcurrent or redis.call('ZCARD', userSlots) >= maxConcurrentPerUser then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

-- 2. Reap expired org entries from orgMeta/orgIdx/userIdx (declared keys only).
local evicted = reap_org_meta(orgIdx, orgMeta, userIdx, oKey, nowMs)

if redis.call('EXISTS', jobKey) == 1 then
	return {'ERR', 'DUPLICATE_JOB'}
end

-- 3. Transactional capacity planning: choose all victims BEFORE deleting anything.
local orgMembers = redis.call('ZRANGE', orgIdx, 0, -1)
local callerInactive = {}
local otherOrgInactive = {}
local userCount = 0
local orgCount = #orgMembers

for _, id in ipairs(orgMembers) do
	local mOKey, _, actMs = parse_meta(redis.call('HGET', orgMeta, id))
	local active = meta_is_active(actMs, nowMs)
	if mOKey == oKey then
		userCount = userCount + 1
		if not active then
			table.insert(callerInactive, {id = id, oKey = mOKey})
		end
	elseif not active then
		table.insert(otherOrgInactive, {id = id, oKey = mOKey or ''})
	end
end

local needUserEvict = 0
if userCount >= maxJobsPerUser then
	needUserEvict = userCount - maxJobsPerUser + 1
end
if needUserEvict > #callerInactive then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local victims = {}
for i = 1, needUserEvict do
	table.insert(victims, callerInactive[i])
end

local remainingCaller = #callerInactive - needUserEvict
local remainingOrgCount = orgCount - needUserEvict
local needOrgEvict = 0
if remainingOrgCount >= maxJobs then
	needOrgEvict = remainingOrgCount - maxJobs + 1
end
if needOrgEvict > remainingCaller + #otherOrgInactive then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local cIdx = needUserEvict + 1
while needOrgEvict > 0 and cIdx <= #callerInactive do
	table.insert(victims, callerInactive[cIdx])
	cIdx = cIdx + 1
	needOrgEvict = needOrgEvict - 1
end
local oIdx = 1
while needOrgEvict > 0 and oIdx <= #otherOrgInactive do
	table.insert(victims, otherOrgInactive[oIdx])
	oIdx = oIdx + 1
	needOrgEvict = needOrgEvict - 1
end

-- Apply planned evictions atomically on declared keys.
for _, v in ipairs(victims) do
	redis.call('ZREM', orgIdx, v.id)
	redis.call('HDEL', orgMeta, v.id)
	if v.oKey == oKey then
		redis.call('ZREM', userIdx, v.id)
	end
	table.insert(evicted, v.id .. '|' .. v.oKey)
end

local leaseUntilMs = nowMs + leaseTTLMs
local expiresAtMs = nowMs + jobTTLMs
local keyTTLMs = math.max(jobTTLMs, leaseTTLMs)

redis.call('HSET', jobKey,
	'id', jobID,
	'org_id', orgID,
	'user_login', userLogin,
	'datasource_uid', dsUID,
	'query_kind', qKind,
	'scope_json', scopeJSON,
	'import_source_json', '',
	'status', initStatus,
	'job_kind', jobKind,
	'report_json', '',
	'current_ir_json', '',
	'current_revision', '0',
	'panel_ids', '',
	'hist_len', '0',
	'last_diff_json', '',
	'summary', '',
	'warnings_json', '',
	'error_message', '',
	'repair_attempts_used', '0',
	'max_repair_attempts', maxRepairs,
	'llm_meta_json', '',
	'known_gauges', merge_lines_bounded('', knownGauges, maxGauges),
	'created_at_ms', tostring(nowMs),
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs),
	'active_op_token', '1',
	'active_op_until_ms', tostring(leaseUntilMs)
)
safe_pexpire(jobKey, keyTTLMs)

redis.call('SADD', orgsSet, orgID)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, leaseUntilMs))
extend_pexpire(orgsSet, keyTTLMs + idxBufMs)
extend_pexpire(orgIdx, keyTTLMs + idxBufMs)
extend_pexpire(orgMeta, keyTTLMs + idxBufMs)
extend_pexpire(userIdx, keyTTLMs + idxBufMs)

redis.call('ZADD', orgSlots, leaseUntilMs, slotID)
redis.call('ZADD', userSlots, leaseUntilMs, slotID)
extend_pexpire(orgSlots, leaseTTLMs + idxBufMs)
extend_pexpire(userSlots, leaseTTLMs + idxBufMs)

return {'OK', '1', evicted}
`)

var redisScriptCreateImported = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]
local orgsSet = KEYS[5]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local dsUID = ARGV[5]
local qKind = ARGV[6]
local scopeJSON = ARGV[7]
local importSrcJSON = ARGV[8]
local draftJSON = ARGV[9]
local draftRev = ARGV[10]
local panelIDs = ARGV[11]
local summary = ARGV[12]
local warnsJSON = ARGV[13]
local maxRepairs = ARGV[14]
local knownGauges = ARGV[15]
local nowMs = resolve_now_ms(tonumber(ARGV[16]) or 0)
local jobTTLMs = tonumber(ARGV[17]) or 1
local maxJobs = tonumber(ARGV[18]) or 1
local maxJobsPerUser = tonumber(ARGV[19]) or 1
local idxBufMs = tonumber(ARGV[20]) or 60000
local maxGauges = tonumber(ARGV[21]) or 512

local evicted = reap_org_meta(orgIdx, orgMeta, userIdx, oKey, nowMs)

if redis.call('EXISTS', jobKey) == 1 then
	return {'ERR', 'DUPLICATE_JOB'}
end

-- Import MUST NEVER evict another user's non-expired job (prevents cross-user eviction DoS).
local orgMembers = redis.call('ZRANGE', orgIdx, 0, -1)
local callerInactive = {}
local userCount = 0
local orgCount = #orgMembers

for _, id in ipairs(orgMembers) do
	local mOKey, _, actMs = parse_meta(redis.call('HGET', orgMeta, id))
	if mOKey == oKey then
		userCount = userCount + 1
		if not meta_is_active(actMs, nowMs) then
			table.insert(callerInactive, id)
		end
	end
end

local needUserEvict = 0
if userCount >= maxJobsPerUser then
	needUserEvict = userCount - maxJobsPerUser + 1
end
local needOrgEvict = 0
if orgCount >= maxJobs then
	needOrgEvict = orgCount - maxJobs + 1
end
local needTotal = math.max(needUserEvict, needOrgEvict)
if needTotal > #callerInactive then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

for i = 1, needTotal do
	local victimID = callerInactive[i]
	redis.call('ZREM', orgIdx, victimID)
	redis.call('ZREM', userIdx, victimID)
	redis.call('HDEL', orgMeta, victimID)
	table.insert(evicted, victimID .. '|' .. oKey)
end

local expiresAtMs = nowMs + jobTTLMs

redis.call('HSET', jobKey,
	'id', jobID,
	'org_id', orgID,
	'user_login', userLogin,
	'datasource_uid', dsUID,
	'query_kind', qKind,
	'scope_json', scopeJSON,
	'import_source_json', importSrcJSON,
	'status', 'awaiting_preview',
	'current_ir_json', draftJSON,
	'current_revision', draftRev,
	'panel_ids', panelIDs,
	'hist_len', '0',
	'last_diff_json', '',
	'summary', summary,
	'warnings_json', warnsJSON,
	'error_message', '',
	'repair_attempts_used', '0',
	'max_repair_attempts', maxRepairs,
	'llm_meta_json', '',
	'known_gauges', merge_lines_bounded('', knownGauges, maxGauges),
	'created_at_ms', tostring(nowMs),
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs),
	'active_op_token', '0',
	'active_op_until_ms', '0'
)
safe_pexpire(jobKey, jobTTLMs)

redis.call('SADD', orgsSet, orgID)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgsSet, jobTTLMs + idxBufMs)
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', read_snapshot(jobKey), evicted}
`)

var redisScriptGetJob = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local nowMs = resolve_now_ms(tonumber(ARGV[4]) or 0)

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

return {'OK', read_snapshot(jobKey)}
`)

var redisScriptGetRepairCandidate = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local nowMs = resolve_now_ms(tonumber(ARGV[4]) or 0)

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

if hget_str(jobKey, 'job_kind') == 'report' then
	return {'ERR', 'WRONG_JOB_KIND'}
end

local vals = redis.call('HMGET', jobKey,
	'current_ir_json',
	'current_revision',
	'repair_attempts_used',
	'max_repair_attempts',
	'active_op_until_ms',
	'query_kind'
)
return {
	'OK',
	vals[1] or '',
	vals[2] or '0',
	vals[3] or '0',
	vals[4] or '0',
	vals[5] or '0',
	vals[6] or '',
	tostring(nowMs)
}
`)

var redisScriptBeginPatch = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]
local orgSlots = KEYS[5]
local userSlots = KEYS[6]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local baseRev = tonumber(ARGV[5]) or 0
local targetPanelID = ARGV[6]
local catalogGauges = ARGV[7]
local slotID = ARGV[8]
local nowMs = resolve_now_ms(tonumber(ARGV[9]) or 0)
local jobTTLMs = tonumber(ARGV[10]) or 1
local leaseTTLMs = tonumber(ARGV[11]) or 1
local maxConcurrent = tonumber(ARGV[12]) or 1
local maxConcurrentPerUser = tonumber(ARGV[13]) or 1
local idxBufMs = tonumber(ARGV[14]) or 60000
local maxGauges = tonumber(ARGV[15]) or 512

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

if hget_str(jobKey, 'job_kind') == 'report' then
	return {'ERR', 'WRONG_JOB_KIND'}
end

local currentIR = hget_str(jobKey, 'current_ir_json')
if currentIR == '' then
	return {'ERR', 'NO_DRAFT'}
end

local currentRev = hget_num(jobKey, 'current_revision')
if currentRev ~= baseRev then
	return {'ERR', 'REVISION_CONFLICT', tostring(currentRev)}
end

if targetPanelID ~= '' then
	local panelIDs = hget_str(jobKey, 'panel_ids')
	if not contains_line(panelIDs, targetPanelID) then
		return {'ERR', 'PANEL_NOT_FOUND'}
	end
end

if is_active(jobKey, nowMs) then
	return {'ERR', 'OP_IN_PROGRESS'}
end

redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
if redis.call('ZCARD', orgSlots) >= maxConcurrent or redis.call('ZCARD', userSlots) >= maxConcurrentPerUser then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local existingGauges = hget_str(jobKey, 'known_gauges')
local mergedGauges = merge_lines_bounded(existingGauges, catalogGauges, maxGauges)
local nextToken = hget_num(jobKey, 'active_op_token') + 1
local leaseUntilMs = nowMs + leaseTTLMs
local expiresAtMs = nowMs + jobTTLMs
local keyTTLMs = math.max(jobTTLMs, leaseTTLMs)

redis.call('HSET', jobKey,
	'known_gauges', mergedGauges,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', tostring(leaseUntilMs),
	'status', 'generating',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, keyTTLMs)

redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, leaseUntilMs))
extend_pexpire(orgIdx, keyTTLMs + idxBufMs)
extend_pexpire(orgMeta, keyTTLMs + idxBufMs)
extend_pexpire(userIdx, keyTTLMs + idxBufMs)

redis.call('ZADD', orgSlots, leaseUntilMs, slotID)
redis.call('ZADD', userSlots, leaseUntilMs, slotID)
extend_pexpire(orgSlots, leaseTTLMs + idxBufMs)
extend_pexpire(userSlots, leaseTTLMs + idxBufMs)

local scopeJSON = hget_str(jobKey, 'scope_json')
local qKind = hget_str(jobKey, 'query_kind')
return {'OK', tostring(nextToken), currentIR, tostring(currentRev), scopeJSON, qKind, mergedGauges}
`)

var redisScriptBeginRepair = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]
local orgSlots = KEYS[5]
local userSlots = KEYS[6]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local baseRev = tonumber(ARGV[5]) or 0
local catalogGauges = ARGV[6]
local slotID = ARGV[7]
local nowMs = resolve_now_ms(tonumber(ARGV[8]) or 0)
local jobTTLMs = tonumber(ARGV[9]) or 1
local leaseTTLMs = tonumber(ARGV[10]) or 1
local maxConcurrent = tonumber(ARGV[11]) or 1
local maxConcurrentPerUser = tonumber(ARGV[12]) or 1
local idxBufMs = tonumber(ARGV[13]) or 60000
local maxGauges = tonumber(ARGV[14]) or 512

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

if hget_str(jobKey, 'job_kind') == 'report' then
	return {'ERR', 'WRONG_JOB_KIND'}
end

local currentIR = hget_str(jobKey, 'current_ir_json')
if currentIR == '' then
	return {'ERR', 'NO_DRAFT'}
end

local currentRev = hget_num(jobKey, 'current_revision')
if currentRev ~= baseRev then
	return {'ERR', 'REVISION_CONFLICT', tostring(currentRev)}
end

if is_active(jobKey, nowMs) then
	return {'ERR', 'OP_IN_PROGRESS'}
end

local used = hget_num(jobKey, 'repair_attempts_used')
local maxRepairs = hget_num(jobKey, 'max_repair_attempts')
if used >= maxRepairs then
	return {'ERR', 'BUDGET_EXHAUSTED', tostring(used), tostring(maxRepairs)}
end

redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
if redis.call('ZCARD', orgSlots) >= maxConcurrent or redis.call('ZCARD', userSlots) >= maxConcurrentPerUser then
	return {'ERR', 'CONCURRENCY_LIMITED'}
end

local existingGauges = hget_str(jobKey, 'known_gauges')
local mergedGauges = merge_lines_bounded(existingGauges, catalogGauges, maxGauges)
local nextToken = hget_num(jobKey, 'active_op_token') + 1
local nextUsed = used + 1
local leaseUntilMs = nowMs + leaseTTLMs
local expiresAtMs = nowMs + jobTTLMs
local keyTTLMs = math.max(jobTTLMs, leaseTTLMs)

redis.call('HSET', jobKey,
	'known_gauges', mergedGauges,
	'repair_attempts_used', tostring(nextUsed),
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', tostring(leaseUntilMs),
	'status', 'repairing',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, keyTTLMs)

redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, leaseUntilMs))
extend_pexpire(orgIdx, keyTTLMs + idxBufMs)
extend_pexpire(orgMeta, keyTTLMs + idxBufMs)
extend_pexpire(userIdx, keyTTLMs + idxBufMs)

redis.call('ZADD', orgSlots, leaseUntilMs, slotID)
redis.call('ZADD', userSlots, leaseUntilMs, slotID)
extend_pexpire(orgSlots, leaseTTLMs + idxBufMs)
extend_pexpire(userSlots, leaseTTLMs + idxBufMs)

local scopeJSON = hget_str(jobKey, 'scope_json')
local qKind = hget_str(jobKey, 'query_kind')
return {'OK', tostring(nextToken), currentIR, tostring(currentRev), scopeJSON, qKind, mergedGauges}
`)

var redisScriptSetStatus = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local status = ARGV[6]
local nowMs = resolve_now_ms(tonumber(ARGV[7]) or 0)
local jobTTLMs = tonumber(ARGV[8]) or 1
local idxBufMs = tonumber(ARGV[9]) or 60000

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
local untilMs = hget_num(jobKey, 'active_op_until_ms')
if opToken > 0 and storedToken == opToken and untilMs > nowMs then
	local remLeaseMs = math.max(0, untilMs - nowMs)
	local keyTTLMs = math.max(jobTTLMs, remLeaseMs)
	local expiresAtMs = nowMs + jobTTLMs
	redis.call('HSET', jobKey,
		'status', status,
		'updated_at_ms', tostring(nowMs),
		'expires_at_ms', tostring(expiresAtMs)
	)
	safe_pexpire(jobKey, keyTTLMs)
	redis.call('ZADD', orgIdx, nowMs, jobID)
	redis.call('ZADD', userIdx, nowMs, jobID)
	redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, untilMs))
	extend_pexpire(orgIdx, keyTTLMs + idxBufMs)
	extend_pexpire(orgMeta, keyTTLMs + idxBufMs)
	extend_pexpire(userIdx, keyTTLMs + idxBufMs)
end
return {'OK'}
`)

var redisScriptFinishJob = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local status = ARGV[6]
local hasDoc = ARGV[7]
local docJSON = ARGV[8]
local docRev = ARGV[9]
local panelIDs = ARGV[10]
local diffsJSON = ARGV[11]
local summary = ARGV[12]
local warnsJSON = ARGV[13]
local errMsg = ARGV[14]
local hasMeta = ARGV[15]
local metaJSON = ARGV[16]
local nowMs = resolve_now_ms(tonumber(ARGV[17]) or 0)
local jobTTLMs = tonumber(ARGV[18]) or 1
local idxBufMs = tonumber(ARGV[19]) or 60000
local hasReport = ARGV[20] or '0'
local reportJSON = ARGV[21] or ''

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
if opToken <= 0 or storedToken ~= opToken or not is_active(jobKey, nowMs) then
	return {'CANCELLED', read_snapshot(jobKey)}
end

local expiresAtMs = nowMs + jobTTLMs
local nextToken = storedToken + 1
redis.call('HSET', jobKey,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'status', status,
	'last_diff_json', diffsJSON,
	'summary', summary,
	'warnings_json', warnsJSON,
	'error_message', errMsg,
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
if hasDoc == '1' then
	redis.call('HSET', jobKey,
		'current_ir_json', docJSON,
		'current_revision', docRev,
		'panel_ids', panelIDs
	)
end
if hasReport == '1' then
	redis.call('HSET', jobKey, 'report_json', reportJSON)
end
if hasMeta == '1' then
	redis.call('HSET', jobKey, 'llm_meta_json', metaJSON)
end

safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', read_snapshot(jobKey)}
`)

var redisScriptCommitRevision = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local expectedRev = tonumber(ARGV[6]) or 0
local nextIRJSON = ARGV[7]
local nextRev = ARGV[8]
local nextPanelIDs = ARGV[9]
local diffsJSON = ARGV[10]
local summary = ARGV[11]
local warnsJSON = ARGV[12]
local hasMeta = ARGV[13]
local metaJSON = ARGV[14]
local maxHist = tonumber(ARGV[15]) or 10
local nowMs = resolve_now_ms(tonumber(ARGV[16]) or 0)
local jobTTLMs = tonumber(ARGV[17]) or 1
local idxBufMs = tonumber(ARGV[18]) or 60000

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
if opToken <= 0 or storedToken ~= opToken or not is_active(jobKey, nowMs) then
	return {'CANCELLED', read_snapshot(jobKey)}
end

local prevIR = hget_str(jobKey, 'current_ir_json')
local curRev = hget_num(jobKey, 'current_revision')
if prevIR == '' or curRev ~= expectedRev then
	redis.call('HSET', jobKey,
		'active_op_token', tostring(storedToken + 1),
		'active_op_until_ms', '0'
	)
	local expMs = hget_num(jobKey, 'expires_at_ms')
	redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expMs, 0))
	return {'CONFLICT', read_snapshot(jobKey)}
end

local prevPanels = hget_str(jobKey, 'panel_ids')
local histLen = hget_num(jobKey, 'hist_len')
redis.call('HSET', jobKey,
	'hist_' .. tostring(histLen), prevIR,
	'hist_rev_' .. tostring(histLen), tostring(curRev),
	'hist_panels_' .. tostring(histLen), prevPanels
)
histLen = histLen + 1
if histLen > maxHist then
	local shift = histLen - maxHist
	for i = 0, maxHist - 1 do
		local src = tostring(i + shift)
		local dst = tostring(i)
		redis.call('HSET', jobKey,
			'hist_' .. dst, hget_str(jobKey, 'hist_' .. src),
			'hist_rev_' .. dst, tostring(hget_num(jobKey, 'hist_rev_' .. src)),
			'hist_panels_' .. dst, hget_str(jobKey, 'hist_panels_' .. src)
		)
	end
	for d = maxHist, histLen - 1 do
		local drop = tostring(d)
		redis.call('HDEL', jobKey, 'hist_' .. drop, 'hist_rev_' .. drop, 'hist_panels_' .. drop)
	end
	histLen = maxHist
end

local expiresAtMs = nowMs + jobTTLMs
local nextToken = storedToken + 1
redis.call('HSET', jobKey,
	'hist_len', tostring(histLen),
	'current_ir_json', nextIRJSON,
	'current_revision', nextRev,
	'panel_ids', nextPanelIDs,
	'last_diff_json', diffsJSON,
	'summary', summary,
	'warnings_json', warnsJSON,
	'error_message', '',
	'status', 'awaiting_preview',
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
if hasMeta == '1' then
	redis.call('HSET', jobKey, 'llm_meta_json', metaJSON)
end

safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', read_snapshot(jobKey)}
`)

var redisScriptRecordEditFailure = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local errMsg = ARGV[6]
local hasMeta = ARGV[7]
local metaJSON = ARGV[8]
local nowMs = resolve_now_ms(tonumber(ARGV[9]) or 0)
local jobTTLMs = tonumber(ARGV[10]) or 1
local idxBufMs = tonumber(ARGV[11]) or 60000

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
if opToken <= 0 or storedToken ~= opToken or not is_active(jobKey, nowMs) then
	return {'CANCELLED', read_snapshot(jobKey)}
end

local expiresAtMs = nowMs + jobTTLMs
local nextToken = storedToken + 1
redis.call('HSET', jobKey,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'status', 'failed',
	'error_message', errMsg,
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
if hasMeta == '1' then
	redis.call('HSET', jobKey, 'llm_meta_json', metaJSON)
end

safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

return {'OK', read_snapshot(jobKey)}
`)

var redisScriptRevertOnCancel = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local opToken = tonumber(ARGV[5]) or 0
local cancelSummary = ARGV[6]
local nowMs = resolve_now_ms(tonumber(ARGV[7]) or 0)
local jobTTLMs = tonumber(ARGV[8]) or 1
local idxBufMs = tonumber(ARGV[9]) or 60000

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local storedToken = hget_num(jobKey, 'active_op_token')
if opToken > 0 and storedToken == opToken and is_active(jobKey, nowMs) then
	local jobKind = hget_str(jobKey, 'job_kind')
	local currentIR = hget_str(jobKey, 'current_ir_json')
	local nextStatus = 'cancelled'
	local nextSummary = cancelSummary
	if jobKind == 'report' then
		nextStatus = 'cancelled'
		nextSummary = 'Status analysis was cancelled.'
	elseif currentIR ~= '' then
		nextStatus = 'awaiting_preview'
	end
	local expiresAtMs = nowMs + jobTTLMs
	local nextToken = storedToken + 1
	redis.call('HSET', jobKey,
		'active_op_token', tostring(nextToken),
		'active_op_until_ms', '0',
		'status', nextStatus,
		'summary', nextSummary,
		'updated_at_ms', tostring(nowMs),
		'expires_at_ms', tostring(expiresAtMs)
	)
	safe_pexpire(jobKey, jobTTLMs)
	redis.call('ZADD', orgIdx, nowMs, jobID)
	redis.call('ZADD', userIdx, nowMs, jobID)
	redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
	extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
	extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
	extend_pexpire(userIdx, jobTTLMs + idxBufMs)
end

return {'CANCELLED', read_snapshot(jobKey)}
`)

var redisScriptUndo = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local maxRev = tonumber(ARGV[5]) or 1000
local undoSummary = ARGV[6]
local nowMs = resolve_now_ms(tonumber(ARGV[7]) or 0)
local jobTTLMs = tonumber(ARGV[8]) or 1
local idxBufMs = tonumber(ARGV[9]) or 60000
local cancelChannel = ARGV[10]

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

if hget_str(jobKey, 'job_kind') == 'report' then
	return {'ERR', 'WRONG_JOB_KIND'}
end

local histLen = hget_num(jobKey, 'hist_len')
local currentIR = hget_str(jobKey, 'current_ir_json')
if histLen <= 0 or currentIR == '' then
	return {'ERR', 'NO_UNDO_HISTORY'}
end

local oldRev = hget_num(jobKey, 'current_revision')
if oldRev >= maxRev then
	return {'ERR', 'REVISION_LIMIT'}
end

local idx = histLen - 1
local idxStr = tostring(idx)
local prevIR = hget_str(jobKey, 'hist_' .. idxStr)
local prevRev = hget_num(jobKey, 'hist_rev_' .. idxStr)
local prevPanels = hget_str(jobKey, 'hist_panels_' .. idxStr)
redis.call('HDEL', jobKey, 'hist_' .. idxStr, 'hist_rev_' .. idxStr, 'hist_panels_' .. idxStr)

local newRev = oldRev + 1
local nextToken = hget_num(jobKey, 'active_op_token') + 1
local diffJSON = string.format(
	'[{"op":"undo","field":"revision","before":"rev %d","after":"restored from rev %d as rev %d"}]',
	oldRev, prevRev, newRev
)
local expiresAtMs = nowMs + jobTTLMs

redis.call('HSET', jobKey,
	'hist_len', tostring(idx),
	'current_ir_json', prevIR,
	'current_revision', tostring(newRev),
	'panel_ids', prevPanels,
	'last_diff_json', diffJSON,
	'summary', undoSummary,
	'status', 'awaiting_preview',
	'error_message', '',
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

if cancelChannel and cancelChannel ~= '' then
	redis.call('PUBLISH', cancelChannel, jobID .. ':' .. tostring(nextToken))
end

return {'OK', read_snapshot(jobKey), tostring(nextToken)}
`)

var redisScriptCancel = redis.NewScript(luaCommonHelpers + `
local jobKey = KEYS[1]
local orgIdx = KEYS[2]
local userIdx = KEYS[3]
local orgMeta = KEYS[4]

local jobID = ARGV[1]
local orgID = ARGV[2]
local userLogin = ARGV[3]
local oKey = ARGV[4]
local cancelSummary = ARGV[5]
local nowMs = resolve_now_ms(tonumber(ARGV[6]) or 0)
local jobTTLMs = tonumber(ARGV[7]) or 1
local idxBufMs = tonumber(ARGV[8]) or 60000
local cancelChannel = ARGV[9]

local errReply = check_owned_job(jobKey, orgIdx, userIdx, orgMeta, jobID, orgID, userLogin, nowMs)
if errReply then
	return errReply
end

local jobKind = hget_str(jobKey, 'job_kind')
local curStatus = hget_str(jobKey, 'status')
if jobKind == 'report' and not is_active(jobKey, nowMs) and (curStatus == 'ready' or curStatus == 'failed' or curStatus == 'cancelled') then
	return {'OK', read_snapshot(jobKey), tostring(hget_num(jobKey, 'active_op_token'))}
end

local nextToken = hget_num(jobKey, 'active_op_token') + 1
local currentIR = hget_str(jobKey, 'current_ir_json')
local nextStatus = 'cancelled'
local nextSummary = cancelSummary
if jobKind == 'report' then
	nextStatus = 'cancelled'
	nextSummary = 'Status analysis was cancelled.'
elseif currentIR ~= '' then
	nextStatus = 'awaiting_preview'
end
local expiresAtMs = nowMs + jobTTLMs

redis.call('HSET', jobKey,
	'active_op_token', tostring(nextToken),
	'active_op_until_ms', '0',
	'status', nextStatus,
	'summary', nextSummary,
	'updated_at_ms', tostring(nowMs),
	'expires_at_ms', tostring(expiresAtMs)
)
safe_pexpire(jobKey, jobTTLMs)
redis.call('ZADD', orgIdx, nowMs, jobID)
redis.call('ZADD', userIdx, nowMs, jobID)
redis.call('HSET', orgMeta, jobID, encode_meta(oKey, expiresAtMs, 0))
extend_pexpire(orgIdx, jobTTLMs + idxBufMs)
extend_pexpire(orgMeta, jobTTLMs + idxBufMs)
extend_pexpire(userIdx, jobTTLMs + idxBufMs)

if cancelChannel and cancelChannel ~= '' then
	redis.call('PUBLISH', cancelChannel, jobID .. ':' .. tostring(nextToken))
end

return {'OK', read_snapshot(jobKey), tostring(nextToken)}
`)

var redisScriptReleaseSlot = redis.NewScript(luaCommonHelpers + `
local orgSlots = KEYS[1]
local userSlots = KEYS[2]
local slotID = ARGV[1]
local nowMs = resolve_now_ms(tonumber(ARGV[2]) or 0)

redis.call('ZREM', orgSlots, slotID)
redis.call('ZREM', userSlots, slotID)
redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
redis.call('ZREMRANGEBYSCORE', userSlots, '-inf', nowMs)
return 1
`)

var redisScriptCleanupOrg = redis.NewScript(luaCommonHelpers + `
local orgIdx = KEYS[1]
local orgMeta = KEYS[2]
local orgSlots = KEYS[3]
local nowMs = resolve_now_ms(tonumber(ARGV[1]) or 0)

redis.call('ZREMRANGEBYSCORE', orgSlots, '-inf', nowMs)
return reap_org_meta(orgIdx, orgMeta, nil, '', nowMs)
`)
