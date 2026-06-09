package io.openthesis;

import java.util.Map;

public class Lifecycle {

    public static void setupComplete(Map<String, Object> details) {
        StringBuilder sb = new StringBuilder();
        sb.append("{\"openthesis_setup_complete\":{");
        sb.append("\"status\":\"complete\"");
        if (details != null && !details.isEmpty()) {
            sb.append(",\"details\":").append(Assert.mapToJson(details));
        } else {
            sb.append(",\"details\":{}");
        }
        sb.append("}}");
        SdkWriter.emit(sb.toString());
    }

    public static void sendEvent(String eventName, Map<String, Object> details) {
        StringBuilder sb = new StringBuilder();
        sb.append("{\"openthesis_send_event\":{");
        sb.append("\"event_name\":").append(Assert.jsonStr(eventName));
        if (details != null && !details.isEmpty()) {
            sb.append(",\"details\":").append(Assert.mapToJson(details));
        } else {
            sb.append(",\"details\":{}");
        }
        sb.append("}}");
        SdkWriter.emit(sb.toString());
    }

    public static void teardown(Map<String, Object> details) {
        StringBuilder sb = new StringBuilder();
        sb.append("{\"openthesis_teardown\":{");
        sb.append("\"status\":\"complete\"");
        if (details != null && !details.isEmpty()) {
            sb.append(",\"details\":").append(Assert.mapToJson(details));
        } else {
            sb.append(",\"details\":{}");
        }
        sb.append("}}");
        SdkWriter.emit(sb.toString());
    }

    public static void stopFaults(double durationSeconds) {
        StringBuilder sb = new StringBuilder();
        sb.append("{\"openthesis_stop_faults\":{");
        sb.append("\"duration_seconds\":").append(durationSeconds).append(",");
        sb.append("\"status\":\"requested\"");
        sb.append("}}");
        SdkWriter.emit(sb.toString());
    }
}
