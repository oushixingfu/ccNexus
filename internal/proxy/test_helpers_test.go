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
