package proxy

type noopStatsStorage struct{}

func (noopStatsStorage) RecordDailyStat(stat interface{}) error { return nil }
func (noopStatsStorage) GetTotalStats() (int, map[string]interface{}, error) {
	return 0, map[string]interface{}{}, nil
}
func (noopStatsStorage) GetDailyStats(endpointName, startDate, endDate string) ([]interface{}, error) {
	return nil, nil
}
func (noopStatsStorage) GetPeriodStatsAggregated(startDate, endDate string) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

type recordingStatsStorage struct {
	records []StatRecord
}

func (s *recordingStatsStorage) RecordDailyStat(stat interface{}) error {
	switch v := stat.(type) {
	case *StatRecord:
		s.records = append(s.records, *v)
	case StatRecord:
		s.records = append(s.records, v)
	}
	return nil
}

func (s *recordingStatsStorage) GetTotalStats() (int, map[string]interface{}, error) {
	return 0, map[string]interface{}{}, nil
}

func (s *recordingStatsStorage) GetDailyStats(endpointName, startDate, endDate string) ([]interface{}, error) {
	return nil, nil
}

func (s *recordingStatsStorage) GetPeriodStatsAggregated(startDate, endDate string) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (s *recordingStatsStorage) totals() (requests int, errors int, inputTokens int, outputTokens int) {
	for _, record := range s.records {
		requests += record.Requests
		errors += record.Errors
		inputTokens += record.InputTokens
		outputTokens += record.OutputTokens
	}
	return requests, errors, inputTokens, outputTokens
}
