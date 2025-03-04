package hoverfly

import (
	"errors"
	"fmt"
	"github.com/SpectoLabs/hoverfly/core/templating"
	"regexp"
	"sort"

	"github.com/SpectoLabs/hoverfly/core/action"
	"github.com/SpectoLabs/hoverfly/core/delay"

	"strings"

	v1 "github.com/SpectoLabs/hoverfly/core/handlers/v1"
	v2 "github.com/SpectoLabs/hoverfly/core/handlers/v2"
	"github.com/SpectoLabs/hoverfly/core/matching/matchers"
	"github.com/SpectoLabs/hoverfly/core/metrics"
	"github.com/SpectoLabs/hoverfly/core/middleware"
	"github.com/SpectoLabs/hoverfly/core/models"
	"github.com/SpectoLabs/hoverfly/core/modes"
	"github.com/SpectoLabs/hoverfly/core/state"
	"github.com/SpectoLabs/hoverfly/core/util"
	log "github.com/sirupsen/logrus"
)

func (hf *Hoverfly) GetDestination() string {
	return hf.Cfg.Destination
}

// UpdateDestination - updates proxy with new destination regexp
func (hf *Hoverfly) SetDestination(destination string) (err error) {
	_, err = regexp.Compile(destination)
	if err != nil {
		return fmt.Errorf("destination is not a valid regular expression string")
	}

	hf.mu.Lock()
	hf.StopProxy()
	hf.Cfg.Destination = destination
	err = hf.StartProxy()
	hf.mu.Unlock()
	return
}

func (hf *Hoverfly) GetMode() v2.ModeView {
	return hf.modeMap[hf.Cfg.Mode].View()
}

func (hf *Hoverfly) SetMode(mode string) error {
	return hf.SetModeWithArguments(v2.ModeView{
		Mode: mode,
	})
}

func (hf *Hoverfly) canSwitchWebserverMode(modeView v2.ModeView) bool {
	return modeView.Mode != modes.Capture && modeView.Mode != modes.Modify
}

func (hf *Hoverfly) SetModeWithArguments(modeView v2.ModeView) error {

	availableModes := map[string]bool{
		modes.Simulate:   true,
		modes.Capture:    true,
		modes.Modify:     true,
		modes.Synthesize: true,
		modes.Spy:        true,
		modes.Diff:       true,
	}

	if modeView.Mode == "" || !availableModes[modeView.Mode] {
		log.WithFields(log.Fields{
			"mode": modeView.Mode,
		}).Error("Unknown mode")
		return fmt.Errorf("Not a valid mode")
	}

	if hf.Cfg.Webserver && !hf.canSwitchWebserverMode(modeView) {
		log.Errorf("Cannot change the mode of Hoverfly to %s when running as a webserver", modeView.Mode)
		return fmt.Errorf("Cannot change the mode of Hoverfly to %s when running as a webserver", modeView.Mode)
	}

	for _, header := range modeView.Arguments.Headers {
		if header == "*" {
			if len(modeView.Arguments.Headers) > 1 {
				return errors.New("Must provide a list containing only an asterix, or a list containing only headers names")
			}
		}
	}

	matchingStrategy := modeView.Arguments.MatchingStrategy
	if modeView.Mode == modes.Simulate {
		if matchingStrategy == nil {
			matchingStrategy = util.StringToPointer("strongest")
		} else if strings.ToLower(*matchingStrategy) != "strongest" && strings.ToLower(*matchingStrategy) != "first" {
			return errors.New("Only matching strategy of 'first' or 'strongest' is permitted")
		}
	}

	hf.Cfg.SetMode(modeView.Mode)
	if hf.Cfg.GetMode() == "capture" {
		hf.CacheMatcher.FlushCache()
	} else if hf.Cfg.GetMode() == "simulate" || hf.Cfg.GetMode() == "spy" {
		hf.CacheMatcher.PreloadCache(hf.Simulation)
	}

	modeArguments := modes.ModeArguments{
		Headers:            modeView.Arguments.Headers,
		MatchingStrategy:   matchingStrategy,
		Stateful:           modeView.Arguments.Stateful,
		OverwriteDuplicate: modeView.Arguments.OverwriteDuplicate,
		CaptureOnMiss:      modeView.Arguments.CaptureOnMiss,
		CaptureDelay:       modeView.Arguments.CaptureDelay,
	}

	hf.modeMap[hf.Cfg.GetMode()].SetArguments(modeArguments)

	log.WithFields(log.Fields{
		"mode": hf.Cfg.GetMode(),
	}).Info("Mode has been changed")

	return nil
}

func (hf *Hoverfly) GetMiddleware() (string, string, string) {
	script, _ := hf.Cfg.Middleware.GetScript()
	return hf.Cfg.Middleware.Binary, script, hf.Cfg.Middleware.Remote
}

func (hf *Hoverfly) SetMiddleware(binary, script, remote string) error {
	newMiddleware := &middleware.Middleware{}
	if binary == "" && script == "" && remote == "" {
		hf.Cfg.Middleware = *newMiddleware
		return nil
	}

	if binary == "" && script != "" {
		return fmt.Errorf("cannot run script with no binary")
	}

	err := newMiddleware.SetBinary(binary)
	if err != nil {
		return err
	}

	err = newMiddleware.SetScript(script)
	if err != nil {
		return err
	}

	err = newMiddleware.SetRemote(remote)
	if err != nil {
		return err
	}

	testData := models.RequestResponsePair{
		Request: models.RequestDetails{
			Path:        "/",
			Method:      "GET",
			Destination: "www.test.com",
			Scheme:      "",
			Query:       map[string][]string{},
			Body:        "",
			Headers:     map[string][]string{"test_header": {"true"}},
		},
		Response: models.ResponseDetails{
			Status:  200,
			Body:    "ok",
			Headers: map[string][]string{"test_header": {"true"}},
		},
	}

	_, err = newMiddleware.Execute(testData)
	if err != nil {
		return err
	}
	hf.Cfg.Middleware = *newMiddleware
	return nil
}

func (hf *Hoverfly) GetRequestCacheCount() (int, error) {
	return len(hf.Simulation.GetMatchingPairs()), nil
}

func (hf *Hoverfly) GetCache() (v2.CacheView, error) {
	return hf.CacheMatcher.GetAllResponses()
}

func (hf *Hoverfly) FlushCache() error {
	return hf.CacheMatcher.FlushCache()
}

func (hf *Hoverfly) SetResponseDelays(payloadView v1.ResponseDelayPayloadView) error {
	err := models.ValidateResponseDelayPayload(payloadView)
	if err != nil {
		return err
	}

	var responseDelays models.ResponseDelayList

	for _, responseDelayView := range payloadView.Data {
		responseDelays = append(responseDelays, models.ResponseDelay{
			UrlPattern: responseDelayView.UrlPattern,
			HttpMethod: responseDelayView.HttpMethod,
			Delay:      responseDelayView.Delay,
		})
	}

	hf.Simulation.ResponseDelays = &responseDelays
	return nil
}

func (hf *Hoverfly) SetVariables(variables []v2.GlobalVariableViewV5) error {
	err := models.ValidateVariablePayload(variables, hf.templator.GetSupportedMethodMap())
	if err != nil {
		return err
	}

	hf.Simulation.AddVariables(models.ImportVariables(variables))
	return nil
}

func (hf *Hoverfly) SetLiterals(literals []v2.GlobalLiteralViewV5) {

	hf.Simulation.AddLiterals(models.ImportLiterals(literals))
}

func (hf *Hoverfly) SetResponseDelaysLogNormal(payloadView v1.ResponseDelayLogNormalPayloadView) error {
	err := models.ValidateResponseDelayLogNormalPayload(payloadView)
	if err != nil {
		return err
	}

	var responseDelaysLogNormal models.ResponseDelayLogNormalList

	for _, responseDelayView := range payloadView.Data {
		responseDelaysLogNormal = append(responseDelaysLogNormal, models.ResponseDelayLogNormal{
			UrlPattern: responseDelayView.UrlPattern,
			HttpMethod: responseDelayView.HttpMethod,
			Min:        responseDelayView.Min,
			Max:        responseDelayView.Max,
			Mean:       responseDelayView.Mean,
			Median:     responseDelayView.Median,
			DelayGenerator: delay.NewLogNormalGenerator(
				responseDelayView.Min,
				responseDelayView.Max,
				responseDelayView.Mean,
				responseDelayView.Median,
			),
		})
	}

	hf.Simulation.ResponseDelaysLogNormal = &responseDelaysLogNormal
	return nil
}

func (hf *Hoverfly) DeleteResponseDelays() {
	hf.Simulation.ResponseDelays = &models.ResponseDelayList{}
}

func (hf *Hoverfly) DeleteResponseDelaysLogNormal() {
	hf.Simulation.ResponseDelaysLogNormal = &models.ResponseDelayLogNormalList{}
}

func (hf *Hoverfly) GetStats() metrics.Stats {
	return hf.Counter.Flush()
}

func (hf *Hoverfly) GetSimulation() (v2.SimulationViewV5, error) {
	pairViews := make([]v2.RequestMatcherResponsePairViewV5, 0)

	for _, v := range hf.Simulation.GetMatchingPairs() {
		pairViews = append(pairViews, v.BuildView())
	}

	return v2.BuildSimulationView(pairViews,
		hf.Simulation.ResponseDelays.ConvertToResponseDelayPayloadView(),
		hf.Simulation.ResponseDelaysLogNormal.ConvertToResponseDelayLogNormalPayloadView(),
		hf.Simulation.Vars.ConvertToGlobalVariablesPayloadView(),
		hf.Simulation.Literals.ConvertToGlobalLiteralsPayloadView(),
		hf.version), nil
}

func (hf *Hoverfly) GetFilteredSimulation(urlPattern string) (v2.SimulationViewV5, error) {
	pairViews := make([]v2.RequestMatcherResponsePairViewV5, 0)
	regexPattern, err := regexp.Compile(urlPattern)

	if err != nil {
		return v2.SimulationViewV5{}, err
	}

	for _, v := range hf.Simulation.GetMatchingPairs() {

		var urlStringToMatch string
		if v.RequestMatcher.Destination != nil && len(v.RequestMatcher.Destination) != 0 && v.RequestMatcher.Destination[0].Matcher == matchers.Exact {
			urlStringToMatch += v.RequestMatcher.Destination[0].Value.(string)
		}
		if v.RequestMatcher.Path != nil && len(v.RequestMatcher.Path) != 0 && v.RequestMatcher.Path[0].Matcher == matchers.Exact {
			urlStringToMatch += v.RequestMatcher.Path[0].Value.(string)
		}

		if regexPattern.MatchString(urlStringToMatch) {
			pairViews = append(pairViews, v.BuildView())
		}
	}

	return v2.BuildSimulationView(pairViews,
		hf.Simulation.ResponseDelays.ConvertToResponseDelayPayloadView(),
		hf.Simulation.ResponseDelaysLogNormal.ConvertToResponseDelayLogNormalPayloadView(),
		hf.Simulation.Vars.ConvertToGlobalVariablesPayloadView(),
		hf.Simulation.Literals.ConvertToGlobalLiteralsPayloadView(),
		hf.version), nil
}

func (hf *Hoverfly) putOrReplaceSimulation(simulationView v2.SimulationViewV5, overrideExisting bool) v2.SimulationImportResult {
	bodyFilesResult := hf.readResponseBodyFiles(simulationView.RequestResponsePairs)
	if bodyFilesResult.GetError() != nil {
		return bodyFilesResult
	}

	if overrideExisting {
		hf.DeleteSimulation()
	}

	result := hf.importRequestResponsePairViewsWithCustomData(simulationView.DataViewV5.RequestResponsePairs, simulationView.GlobalLiterals, simulationView.GlobalVariables)
	if result.GetError() != nil {
		return result
	}

	if err := hf.SetResponseDelays(v1.ResponseDelayPayloadView{Data: simulationView.GlobalActions.Delays}); err != nil {
		result.SetError(err)
		return result
	}

	if err := hf.SetResponseDelaysLogNormal(v1.ResponseDelayLogNormalPayloadView{Data: simulationView.GlobalActions.DelaysLogNormal}); err != nil {
		result.SetError(err)
		return result
	}

	for _, warning := range bodyFilesResult.WarningMessages {
		result.WarningMessages = append(result.WarningMessages, warning)
	}

	return result
}

func (hf *Hoverfly) ReplaceSimulation(simulationView v2.SimulationViewV5) v2.SimulationImportResult {
	return hf.putOrReplaceSimulation(simulationView, true)
}

func (hf *Hoverfly) PutSimulation(simulationView v2.SimulationViewV5) v2.SimulationImportResult {
	return hf.putOrReplaceSimulation(simulationView, false)
}

func (hf *Hoverfly) DeleteSimulation() {
	hf.Simulation.DeleteMatchingPairsAlongWithCustomData()
	hf.DeleteResponseDelays()
	hf.DeleteResponseDelaysLogNormal()
	hf.FlushCache()
}

func (hf *Hoverfly) GetVersion() string {
	return hf.version
}

func (hf *Hoverfly) GetUpstreamProxy() string {
	return hf.Cfg.UpstreamProxy
}

func (hf *Hoverfly) IsWebServer() bool {

	return hf.Cfg.Webserver
}

func (hf *Hoverfly) IsMiddlewareSet() bool {
	return hf.Cfg.Middleware.IsSet()
}

func (hf *Hoverfly) GetCORS() v2.CORSView {
	cors := hf.Cfg.CORS
	return v2.CORSView{
		Enabled:          cors.Enabled,
		AllowOrigin:      cors.AllowOrigin,
		AllowMethods:     cors.AllowMethods,
		AllowHeaders:     cors.AllowHeaders,
		PreflightMaxAge:  cors.PreflightMaxAge,
		AllowCredentials: cors.AllowCredentials,
		ExposeHeaders:    cors.ExposeHeaders,
	}
}

func (hf *Hoverfly) GetState() map[string]string {
	return hf.state.State
}

func (hf *Hoverfly) SetState(state map[string]string) {
	hf.state.SetState(state)
}

func (hf *Hoverfly) PatchState(toPatch map[string]string) {
	hf.state.PatchState(toPatch)
}

func (hf *Hoverfly) ClearState() {
	hf.state = state.NewState()
}

func (hf *Hoverfly) GetDiff() map[v2.SimpleRequestDefinitionView][]v2.DiffReport {
	return hf.responsesDiff
}

func (hf *Hoverfly) ClearDiff() {
	hf.responsesDiff = make(map[v2.SimpleRequestDefinitionView][]v2.DiffReport)
}

func (hf *Hoverfly) AddDiff(requestView v2.SimpleRequestDefinitionView, diffReport v2.DiffReport) {
	if len(diffReport.DiffEntries) > 0 {
		diffs := hf.responsesDiff[requestView]
		hf.responsesDiff[requestView] = append(diffs, diffReport)
	}
}

func (hf *Hoverfly) GetPACFile() []byte {
	return hf.Cfg.PACFile
}

func (hf *Hoverfly) SetPACFile(pacFile []byte) {
	if len(pacFile) == 0 {
		pacFile = nil
	}
	hf.Cfg.PACFile = pacFile
}

func (hf *Hoverfly) DeletePACFile() {
	hf.Cfg.PACFile = nil
}

func (hf *Hoverfly) GetFilteredDiff(diffFilterView v2.DiffFilterView) map[v2.SimpleRequestDefinitionView][]v2.DiffReport {
	responsesDiff := hf.responsesDiff
	filteredResponsesDiff := make(map[v2.SimpleRequestDefinitionView][]v2.DiffReport)
	for request, diffReports := range responsesDiff {
		for _, diffReport := range diffReports {
			var filteredDiffEntries []v2.DiffReportEntry
			for _, diffEntry := range diffReport.DiffEntries {
				if !needsToExcludeDiffEntry(&diffEntry, &diffFilterView) {
					filteredDiffEntries = append(filteredDiffEntries, diffEntry)
				}
			}
			if len(filteredDiffEntries) == 0 {
				continue
			}
			filteredDiffReport := v2.DiffReport{
				Timestamp:   diffReport.Timestamp,
				DiffEntries: filteredDiffEntries,
			}
			if diffReportArr, ok := filteredResponsesDiff[request]; !ok {
				filteredResponsesDiff[request] = []v2.DiffReport{filteredDiffReport}
			} else {
				diffReportArr = append(diffReportArr, filteredDiffReport)
				filteredResponsesDiff[request] = diffReportArr
			}
		}
	}
	return filteredResponsesDiff
}

func (hf *Hoverfly) GetAllPostServeActions() v2.PostServeActionDetailsView {
	var actions []v2.ActionView
	actionNames := make([]string, 0, len(hf.PostServeActionDetails.Actions))

	// Collect all action names
	for actionName := range hf.PostServeActionDetails.Actions {
		actionNames = append(actionNames, actionName)
	}

	// Sort action names to enforce natural order
	sort.Strings(actionNames)

	// Append actions in sorted order
	for _, actionName := range actionNames {
		action := hf.PostServeActionDetails.Actions[actionName]
		actions = append(actions, action.GetActionView(actionName))
	}

	if hf.PostServeActionDetails.FallbackAction != nil {
		actions = append(actions, hf.PostServeActionDetails.FallbackAction.GetActionView(""))
	}

	return v2.PostServeActionDetailsView{
		Actions: actions,
	}
}

func (hf *Hoverfly) SetLocalPostServeAction(actionName string, binary string, scriptContent string, delayInMs int) error {

	action, err := action.NewLocalAction(binary, scriptContent, delayInMs)
	if err != nil {
		return err
	}
	err = hf.PostServeActionDetails.SetAction(actionName, action)
	if err != nil {
		return err
	}
	log.Info("Local post serve action is set")
	return nil
}

func (hf *Hoverfly) SetRemotePostServeAction(actionName, remote string, delayInMs int) error {

	action, err := action.NewRemoteAction(remote, delayInMs)
	if err != nil {
		return err
	}
	err = hf.PostServeActionDetails.SetAction(actionName, action)
	if err != nil {
		return err
	}
	log.Info("Remote post serve action is set")
	return nil
}

func (hf *Hoverfly) DeletePostServeAction(actionName string) error {

	err := hf.PostServeActionDetails.DeleteAction(actionName)
	if err != nil {
		return err
	}
	return nil
}

func needsToExcludeDiffEntry(diffReportEntry *v2.DiffReportEntry, diffFilterView *v2.DiffFilterView) bool {

	//check for header... headers which are ignored during configuration
	for _, header := range (*diffFilterView).ExcludedHeaders {
		headerField := "header/" + header
		if (*diffReportEntry).Field == headerField {
			return true
		}
	}

	for _, responseField := range (*diffFilterView).ExcludedResponseFields {
		relativeResponseField := getRelativeFieldFromJsonPath(responseField)
		if (*diffReportEntry).Field == relativeResponseField {
			return true
		}
	}
	return false
}

func getRelativeFieldFromJsonPath(responseField string) string {
	return strings.Replace(strings.Replace(responseField, "$.", "body/", -1), ".", "/", -1)
}

func (hf *Hoverfly) SetCsvDataSource(dataSourceName, dataSourceContent string) error {

	dataStore, err := templating.NewCsvDataSource(dataSourceName, dataSourceContent)
	if err != nil {
		return err
	}
	hf.templator.TemplateHelper.TemplateDataSource.SetDataSource(dataSourceName, dataStore)
	return nil
}

func (hf *Hoverfly) DeleteDataSource(dataSourceName string) {

	hf.templator.TemplateHelper.TemplateDataSource.DeleteDataSource(dataSourceName)
}

func (hf *Hoverfly) GetAllDataSources() v2.TemplateDataSourceView {

	var csvDataSourceViews []v2.CSVDataSourceView
	for _, value := range hf.templator.TemplateHelper.TemplateDataSource.GetAllDataSources() {
		csvDataSource, _ := value.GetDataSourceView()
		csvDataSourceViews = append(csvDataSourceViews, csvDataSource)
	}
	return v2.TemplateDataSourceView{DataSources: csvDataSourceViews}
}

func (hf *Hoverfly) AddJournalIndex(indexKey string) error {
	return hf.Journal.AddIndex(indexKey)
}

func (hf *Hoverfly) DeleteJournalIndex(indexKey string) {
	hf.Journal.DeleteIndex(indexKey)
}

func (hf *Hoverfly) GetAllIndexes() []v2.JournalIndexView {
	return hf.Journal.GetAllIndexes()
}
