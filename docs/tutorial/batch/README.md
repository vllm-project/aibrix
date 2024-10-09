# Batch API Tutorial

## Prepare dataset
Before submitting a batch job, you need to prepare input data as a file. 
In this file, each line represents a request. 
The request's format is in json. 
The json should cover multiple attributes as specified here, https://platform.openai.com/docs/guides/batch/getting-started, such as endpoint and completion window. 

## Submit job input data
Before submit input data, we need to construct a driver first. Assuming that the file name of the input data is "one_job_input.json", we can
submit the data as the following. This call returns a job ID and later we rely on this job ID for remaining operations.

```
_driver = BatchDriver()
job_id = _driver.upload_batch_data("./one_job_input.json")
```

## Create and submit batch job
This submits batch job for inference.
One parameter is the endpoint name, which should be consistent with the endpoint given in the input file. 
Another parameter is the time duration for job, after which this job will be considered as expired.

```
_driver.create_job(job_id, "sample_endpoint", "20m")
```

## Check job status
After the job submission, we can check job status using the following operation. This API requires the job ID. 
The returned status might be one of the status: JobStatus.PENDING,JobStatus.IN_PROGRESS, JobStatus.COMPLETED. 

```
status = _driver.get_job_status(job_id)
```

## Retrieve job's results
Lastly, we can retrieve job's results as follows. 
The actual result depends on job's execution status. 
If the job is already completed, the returned results are all the results returned from all requests. 
If the job is not completed, it may contain partial results for the input requests.

```
results = _driver.retrieve_job_result(job_id)
```

