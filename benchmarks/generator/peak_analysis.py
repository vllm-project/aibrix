import pandas as pd
import csv

#############################
########### maas  ###########
#############################

# Read the CSV data into a DataFrame
# data = """
# Time,Total,Success,5xx Error,4xx Error
# 2024-10-14 14:04:00,31.6,31.2,,0.387
# 2024-10-14 14:06:00,32.1,31.6,,0.472
# """

# "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-14 14:04:00/API/qps.csv"

# maas
# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-14 14:04:00/API/qps.csv" # 45.8 / 16 = 2.8625
# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-09 01:34:00/API/qps.csv" # 59.0 / 16 = 3.6875
# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-09 05:34:00/API/qps.csv" # 76.3 / 16 = 4.76875
# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-10 19:50:00/API/qps.csv" # 93.2 / 16 = 5.825
# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-15 18:50:00/API/qps.csv" # 84.1 / 16 = 5.25625
# Create the DataFrame

# df = pd.read_csv(filename)

# # Find the peak value of the "Total" column
# peak_value = df["Total"].max()

# print(f"The peak value of the 'Total' column is: {peak_value}")


# filenames = [
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-14 14:04:00/API/input-len.csv",
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-14 14:04:00/API/output-len.csv",
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-09 01:34:00/API/input-len.csv",
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-09 01:34:00/API/output-len.csv",
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-09 05:34:00/API/input-len.csv",
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-09 05:34:00/API/output-len.csv",
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-10 19:50:00/API/input-len.csv",
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-10 19:50:00/API/output-len.csv",
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-15 18:50:00/API/input-len.csv",
#     "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/maas/scenario-extraction/2024-10-15 18:50:00/API/output-len.csv",
# ]

# The peak value of the 'P99' column is: 11319, mean value of the 'P50' column is: 875.9512195121952
# The peak value of the 'P99' column is: 1148, mean value of the 'P50' column is: 45.12682926829268
# The peak value of the 'P99' column is: 32000, mean value of the 'P50' column is: 1291.0731707317073
# The peak value of the 'P99' column is: 32000, mean value of the 'P50' column is: 63.951219512195124
# The peak value of the 'P99' column is: 32000, mean value of the 'P50' column is: 1248.0975609756097
# The peak value of the 'P99' column is: 32000, mean value of the 'P50' column is: 31.090243902439024
# The peak value of the 'P99' column is: 11380, mean value of the 'P50' column is: 382.5238095238095
# The peak value of the 'P99' column is: 564, mean value of the 'P50' column is: 18.43809523809524
# The peak value of the 'P99' column is: 12446, mean value of the 'P50' column is: 1300.5714285714287
# The peak value of the 'P99' column is: 716, mean value of the 'P50' column is: 48.01904761904762

# Create the DataFrame

# for filename in filenames:
#     df = pd.read_csv(filename)

#     # Find the peak value of the "Total" column
#     median_mean = df["P50"].mean()
#     peak_value = df["P99"].max()

#     print(f"The peak value of the 'P99' column is: {peak_value}, mean value of the 'P50' column is: {median_mean}")


#############################
####### cloudide   ##########
#############################



# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/cr/request-cr.csv" # 5.433 / 16 = 0.3395625
# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/bitsai-qa/request-bitsai-qa.csv" # 0.8332 / 16 = 0.052075
# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/ci-rootcause/request-ci-rootcause.csv" # 0.4329 / 16 = 0.02705625
# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/ci-solution/request-ci-solution.csv" # 0.1666 / 16 = 0.0104125
# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/qa-flywheel/request-qa-flywheel.csv" # 8.2984 / 16 = 0.51865
# df = pd.read_csv(filename)
# print(df.columns)
# peak_value = df["bitsai-code-llm-v2.byted.org"].max()
# print(f"The peak value of the 'Total' column is: {peak_value}")


# filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/cr/io-cr.csv" # output 9380.0996 input 325872.3329
# # filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/bitsai-qa/io-bitsai-qa.csv" # output 63081.4997 input 11084.833
# # filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/ci-rootcause/io-ci-rootcause.csv" # output 145213.03309999997 input 38121.4332
# # filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/ci-solution/io-ci-solution.csv" # input 5614.2332
# # filename = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/data/qa-flywheel/io-qa-flywheel.csv" # output 6412.266 input 112725.4653


prefix = "/Users/bytedance/Projects/serverless/aibrix-experiment/aibrix-data/internal-trace/cloudide/2024-12-7 6:30:00/"
dir_suffixes = ["bitsai-qa", "ci-rootcause", "ci-solution", "cr", "qa-flywheel"]
metrics_types=["request", "io"]

# bitsai-qa: The peak value of the 'Total' column is: 0.1332
# bitsai-qa: peak_value_sent: 13315.566600000002 peak_value_recv 1551.9332 sent_to_recv 8.57998694789183
# ci-rootcause: The peak value of the 'Total' column is: 0.0
# ci-rootcause: peak_value_sent: 195.3 peak_value_recv 5048.4331999999995 sent_to_recv 0.038685269718929834
# ci-solution: The peak value of the 'Total' column is: 0.0666
# ci-solution: peak_value_sent: 0.0 peak_value_recv 2816.8999000000003 sent_to_recv 0.0
# cr: The peak value of the 'Total' column is: 0.6998
# cr: peak_value_sent: 1094.4330000000002 peak_value_recv 10071.9665 sent_to_recv 0.10866130263638189
# qa-flywheel: The peak value of the 'Total' column is: 1.7323
# qa-flywheel: peak_value_sent: 3983.4992 peak_value_recv 69533.2321 sent_to_recv 0.05728914189219748

## QPS MAX: 1.7323 (scaling factor 0.10826875)
## recv MAX: 69533.2321 (scaling factor 34)
## sent MAX: 13315.566600000002

for suffix in dir_suffixes:
    for metrics_type in metrics_types:
        filename = f"{prefix}{suffix}/{metrics_type}-{suffix}.csv"
        # print(f"process file {filename}")
        if "io" in metrics_type:
            df = pd.read_csv(filename, parse_dates=['Time'])
            df = df.replace("undefined", 0)
            df['Time'] = pd.to_datetime(df['Time'], unit = 'ms')  # Ensure timestamp is a datetime object
            df = df.set_index('Time')  # Set 'Time' as index for rolling window calculation
            sent_columns = df.filter(regex = r'^sent_bytes.rate@')
            sent_columns = sent_columns.apply(pd.to_numeric, errors='coerce').fillna(0)
            df['sent'] = sent_columns.sum(axis = 1)

            recv_columns = df.filter(regex = r'^recv_bytes.rate@')
            recv_columns = recv_columns.apply(pd.to_numeric, errors='coerce').fillna(0)
            df['recv'] = recv_columns.sum(axis = 1)

            peak_value_sent = df['sent'].max()
            peak_value_recv = df['recv'].max()
            sent_to_recv = peak_value_sent/peak_value_recv
            print(f"{suffix}: peak_value_sent: {peak_value_sent} peak_value_recv {peak_value_recv} sent_to_recv {sent_to_recv}")

        else:
            df = pd.read_csv(filename)
            # print(df.columns)
            peak_value = None
            if "bitsai-code-llm-v2.byted.org" in df:   
                peak_value = df["bitsai-code-llm-v2.byted.org"].max()
            elif "bitsai-code-llm.bytedance.net" in df:
                peak_value = df["bitsai-code-llm.bytedance.net"].max()
            print(f"{suffix}: The peak value of the 'Total' column is: {peak_value}")
