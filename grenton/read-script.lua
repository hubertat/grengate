local reqbody = GATE_HTTP->hb_multi_gate->RequestBody

local resp, state, rl, ix, req

function ReadLight(clu, id)
	local Light = {}

	-- temporary workaround for fibaro wall plug
	if id == "TMP0001" then
		Light.State = CLU_GRENTON_Rs->fib_wall1
		return Light
	end

	if _G[clu]:execute(0, id .. ":get(0)") == 1 then
		Light.State = true
	else
		Light.State = false
	end

	return Light
end

function ReadThermo(clu, thermo, sensor)
	local Thermo = {}

	Thermo.TempMin = _G[clu]:execute(0, thermo .. ":get(10)")
	Thermo.TempMax = _G[clu]:execute(0, thermo .. ":get(11)")
	Thermo.TempTarget = _G[clu]:execute(0, thermo .. ":get(12)")
	Thermo.TempHoliday = _G[clu]:execute(0, thermo .. ":get(4)")
	Thermo.TempSetpoint = _G[clu]:execute(0, thermo .. ":get(3)")
	Thermo.Mode = _G[clu]:execute(0, thermo .. ":get(8)")
	Thermo.State = _G[clu]:execute(0, thermo .. ":get(6)")

	Thermo.TempCurrent = _G[clu]:execute(0, "getVar(\"" .. sensor .. "\")")

	return Thermo
end

function ReadShutter(clu, id)
	local Shutter = {}

	Shutter.MaxTime = _G[clu]:execute(0, id .. ":get(3)")
	Shutter.State = _G[clu]:execute(0, id .. ":get(2)")

	return Shutter
end

function ReadMotionSensor(clu, id)
	local MotionSensor = {}

	if _G[clu]:execute(0, id .. ":get(3)") == 1 then
		MotionSensor.State = true
	else
		MotionSensor.State = false
	end

	return MotionSensor
end


function ReadSwitch(clu, id)
	local Switch = {}

	if _G[clu]:execute(0, id .. ":get(3)") == 1 then
		Switch = true
	else
		Switch = false
	end

	return Switch
end

resp = {}

for ix, req in ipairs(reqbody) do

	if req.Clu ~= nil and _G[req.Clu] ~= nil then
		rl = {}
		rl.Clu = req.Clu
		rl.Id = req.Id
		rl.Kind = req.Kind

		if rl.Kind == "Light" then
			rl.Light = ReadLight(rl.Clu, rl.Id)
		end

		if rl.Kind == "Thermo" then
			rl.Thermo = ReadThermo(rl.Clu, rl.Id, req.Source)
		end

		if rl.Kind == "Shutter" then
			rl.Shutter = ReadShutter(rl.Clu, rl.Id)
		end

		if rl.Kind == "MotionSensor" then
			rl.MotionSensor = ReadMotionSensor(rl.Clu, rl.Id)
		end

		if rl.Kind == "Switch" then
			rl.Switch = ReadSwitch(rl.Clu, rl.Id)
		end

		table.insert(resp, rl)
	end

end

GATE_HTTP->hb_multi_gate->SetResponseBody(resp)
GATE_HTTP->hb_multi_gate->SendResponse()
