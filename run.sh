#!/bin/bash
./cleanup-go --container-name "wp_firstduemovers" --output-csv-path "/var/opt/sites/firstduemovers-spam-results.csv" --analyze-post-content-via-ai --prompt-file ./prompt.sample.txt --post-types "post,page,product" --meta-keys "_yoast_wpseo_metadesc,custom_summary"
